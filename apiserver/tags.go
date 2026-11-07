package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type tagState struct {
	Snapshot *link.SnapshotRef `json:"snapshot,omitempty"`
	// Guest is the guest build of the worker that took the snapshot of a running actor (link.Capabilities.Guest); a
	// template's golden records it (DEFAUL-1606). "" when it was copied from a suspended actor or not declared.
	Guest string `json:"guest,omitempty"`
}

type tagRec struct {
	rec store.Record
	tag *ateapi.Tag
	st  tagState
}

func (s *Server) loadTag(ctx context.Context, space, name string) (*tagRec, error) {
	r, err := s.cfg.Store.Get(ctx, store.Key{Kind: store.KindTag, Space: space, Name: name})
	if err != nil {
		return nil, err
	}
	t, err := decode(r, &ateapi.Tag{})
	if err != nil {
		return nil, err
	}
	tr := &tagRec{rec: r, tag: t}
	if err := json.Unmarshal(r.State, &tr.st); err != nil {
		return nil, fmt.Errorf("apiserver: %v: state: %w", r.Key, err)
	}
	return tr, nil
}

func (tr *tagRec) view() *ateapi.Tag {
	t := proto.Clone(tr.tag).(*ateapi.Tag)
	t.Metadata = meta(tr.rec)
	return t
}

// copySnapshot copies a durable snapshot object to a new key, within the store (snapstore.Store.Copy: server-side
// where the store can), and answers only for a copy that holds the snapshot's bytes.
func (s *Server) copySnapshot(ctx context.Context, from *link.SnapshotRef, key string) (*link.SnapshotRef, error) {
	obj, err := s.cfg.Snapshots.Copy(ctx, from.Key, key, from.Digest)
	if err != nil {
		return nil, err
	}
	if obj.Digest != from.Digest {
		return nil, fmt.Errorf("%w: copy of %s has digest %s, want %s", errNotDurable, from.Key, obj.Digest, from.Digest)
	}
	r := *from
	r.Key = key
	return &r, nil
}

// CreateTag takes a snapshot of source_actor into a new tag (the first half of a fork): a running actor is
// snapshotted in place and keeps running; a suspended one has its latest snapshot copied. The tag exists only once
// its snapshot is verified durable.
func (s *Server) CreateTag(ctx context.Context, req *ateapi.CreateTagRequest) (*ateapi.Tag, error) {
	in := req.GetTag()
	if err := s.authorize(ctx, in.GetMetadata().GetAtespace(), auth.ActionWrite); err != nil {
		return nil, err
	}
	return s.createTag(ctx, in, ateapi.TagScope_TAG_SCOPE_ATESPACE)
}

// createTag is CreateTag once the caller is authorized; scope is the tag's.
func (s *Server) createTag(ctx context.Context, in *ateapi.Tag, scope ateapi.TagScope) (*ateapi.Tag, error) {
	space, name := in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.name is required")
	}
	aspace, aname, err := ref(in.GetSourceActor())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "source_actor: %v", err)
	}
	if aspace != space {
		return nil, status.Errorf(codes.InvalidArgument, "source_actor must be in the tag's atespace %q", space)
	}
	if _, err := s.cfg.Store.Get(ctx, store.Key{Kind: store.KindTag, Space: space, Name: name}); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "tag %s/%s exists", space, name)
	}
	unlock := s.lock(actorKey(aspace, aname))
	defer unlock()
	ar, err := s.loadActor(ctx, aspace, aname)
	if err != nil {
		return nil, toStatus(err)
	}
	nonce, err := store.NewUID()
	if err != nil {
		return nil, toStatus(err)
	}
	key := snapshotKey("tags", space, name, nonce, 1)
	var snap *link.SnapshotRef
	var guest string
	switch ar.actor.Status.GetState() {
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		caps, ok := s.workerCaps(ar.st.Worker)
		if !ok {
			return nil, status.Errorf(codes.Unavailable, "herdlet %q is not connected", ar.st.Worker)
		}
		tpl, _, err := s.loadTemplate(ctx, space, &ateapi.ObjectRef{Name: ar.st.TemplateName})
		if err != nil {
			return nil, toStatus(err)
		}
		guest = caps.Guest
		var res link.Result
		var reason string
		snap, res, reason = s.takeSnapshot(ctx, ar, key, fidelityFor(tpl, caps), "")
		switch res {
		case link.ResultFailed:
			return nil, status.Errorf(codes.FailedPrecondition, "snapshot failed: %s", reason)
		case link.ResultUnknown:
			// The object may exist; remove it so an unknown answer leaves nothing behind, and create no tag.
			_ = s.cfg.Snapshots.Delete(ctx, key)
			return nil, status.Errorf(codes.Unavailable, "snapshot unknown, no tag created: %s", reason)
		}
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		if ar.st.Snapshot == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s has no durable snapshot", aspace, aname)
		}
		if snap, err = s.copySnapshot(ctx, ar.st.Snapshot, key); err != nil {
			return nil, status.Errorf(codes.Unavailable, "copy snapshot: %v", err)
		}
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s is %v", aspace, aname, ar.actor.Status.GetState())
	}
	tag := &ateapi.Tag{
		Scope:       scope,
		SourceActor: &ateapi.ObjectRef{Atespace: aspace, Name: aname},
		Status: &ateapi.TagStatus{
			Snapshot:         snapshotProto(snap, ateapi.SnapshotOwner_SNAPSHOT_OWNER_TAG, 1, ar.st.TemplateUID),
			ActorTemplateUid: ar.st.TemplateUID,
		},
	}
	data, err := proto.Marshal(tag)
	if err != nil {
		return nil, toStatus(err)
	}
	st, err := json.Marshal(tagState{Snapshot: snap, Guest: guest})
	if err != nil {
		return nil, toStatus(err)
	}
	rec, err := s.cfg.Store.Create(ctx, store.Record{Key: store.Key{Kind: store.KindTag, Space: space, Name: name}, Data: data, State: st})
	if err != nil {
		_ = s.cfg.Snapshots.Delete(ctx, key)
		return nil, toStatus(err)
	}
	return (&tagRec{rec: rec, tag: tag}).view(), nil
}

// GetTag returns a tag.
func (s *Server) GetTag(ctx context.Context, req *ateapi.GetTagRequest) (*ateapi.Tag, error) {
	space, name, err := ref(req.GetTag())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionRead); err != nil {
		return nil, err
	}
	tr, err := s.loadTag(ctx, space, name)
	if err != nil {
		return nil, toStatus(err)
	}
	return tr.view(), nil
}

// ListTags lists an atespace's tags by name, as GetTag answers each (scope, source actor, status with the template
// uid); the page token is the last name returned, as for ListActors.
func (s *Server) ListTags(ctx context.Context, req *ateapi.ListTagsRequest) (*ateapi.ListTagsResponse, error) {
	space := req.GetAtespace()
	if err := s.authorize(ctx, space, auth.ActionRead); err != nil {
		return nil, err
	}
	recs, err := s.cfg.Store.List(ctx, store.KindTag, space)
	if err != nil {
		return nil, toStatus(err)
	}
	size := int(req.GetOptions().GetPageSize())
	if size <= 0 || size > 1000 {
		size = 100
	}
	after := req.GetOptions().GetPageToken()
	i := sort.Search(len(recs), func(i int) bool { return recs[i].Name > after })
	resp := &ateapi.ListTagsResponse{}
	for ; i < len(recs) && len(resp.Tags) < size; i++ {
		t, err := decode(recs[i], &ateapi.Tag{})
		if err != nil {
			return nil, toStatus(err)
		}
		resp.Tags = append(resp.Tags, (&tagRec{rec: recs[i], tag: t}).view())
	}
	if i < len(recs) && len(resp.Tags) > 0 {
		resp.NextPageToken = resp.Tags[len(resp.Tags)-1].GetMetadata().GetName()
	}
	return resp, nil
}

// DeleteTag deletes a tag after its snapshot object is verified deleted.
func (s *Server) DeleteTag(ctx context.Context, req *ateapi.DeleteTagRequest) (*ateapi.Tag, error) {
	space, name, err := ref(req.GetTag())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionWrite); err != nil {
		return nil, err
	}
	return s.deleteTag(ctx, space, name, req.GetOptions().GetUid(), req.GetOptions().GetVersion())
}

// deleteTag is DeleteTag once the caller is authorized.
func (s *Server) deleteTag(ctx context.Context, space, name, uid string, version int64) (*ateapi.Tag, error) {
	tr, err := s.loadTag(ctx, space, name)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := precondition(tr.rec, uid, version, false); err != nil {
		return nil, err
	}
	if tr.st.Snapshot != nil {
		if err := s.cfg.Snapshots.Delete(ctx, tr.st.Snapshot.Key); err != nil {
			return nil, status.Errorf(codes.Unavailable, "tag snapshot not verified deleted: %v", err)
		}
	}
	if err := s.cfg.Store.Delete(ctx, tr.rec.Key, tr.rec.UID, tr.rec.Version); err != nil {
		return nil, toStatus(err)
	}
	return tr.view(), nil
}
