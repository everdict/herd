package substrateapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// listTemplatesPage is the page size the list-templates case pages with: small, so a suite's few templates take
// several pages.
const listTemplatesPage = 2

// listTemplates: ListActorTemplates in the suite's atespace, paged to the end, names each template once, each in that
// atespace with a uid and a create_time, and lists the suite's template with the uid GetActorTemplate gave and every
// template the suite created. A target that does not serve it declares a skip.
func (r *run) listTemplates(ctx context.Context) (string, error) {
	listed := map[string]*ateapi.ResourceMetadata{}
	token := ""
	pages := 0
	for ; ; pages++ {
		if pages == 5000 {
			return "", errors.New("ListActorTemplates: still a next_page_token after 5000 pages")
		}
		resp, err := r.c.ListActorTemplates(ctx, r.cfg.Atespace, &ateapi.ListOptions{PageSize: listTemplatesPage, PageToken: token})
		if err := gate("ListActorTemplates", err); err != nil {
			return "", err
		}
		if n := len(resp.GetActorTemplates()); n > listTemplatesPage {
			return "", fmt.Errorf("ListActorTemplates: a page of %d with page_size %d", n, listTemplatesPage)
		}
		for _, t := range resp.GetActorTemplates() {
			md := t.GetMetadata()
			switch {
			case md.GetName() == "":
				return "", fmt.Errorf("ListActorTemplates: a template with no name: %v", md)
			case md.GetAtespace() != r.cfg.Atespace:
				return "", fmt.Errorf("ListActorTemplates %s: template %s is in atespace %q", r.cfg.Atespace, md.GetName(), md.GetAtespace())
			case listed[md.GetName()] != nil:
				return "", fmt.Errorf("ListActorTemplates: template %s listed twice", md.GetName())
			case md.GetUid() == "":
				return "", fmt.Errorf("ListActorTemplates: template %s has no uid", md.GetName())
			case md.GetCreateTime() == nil || md.GetCreateTime().AsTime().IsZero():
				return "", fmt.Errorf("ListActorTemplates: template %s has no create_time", md.GetName())
			}
			listed[md.GetName()] = md
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	own := r.tpl.GetMetadata()
	if md := listed[r.cfg.Template]; md == nil || md.GetUid() != own.GetUid() {
		return "", fmt.Errorf("ListActorTemplates lists the suite's template %s as %v, GetActorTemplate gave uid %s", r.cfg.Template, md, own.GetUid())
	}
	for _, n := range r.templates {
		if listed[n] == nil {
			return "", fmt.Errorf("ListActorTemplates does not list %s, which the suite created", n)
		}
	}
	return fmt.Sprintf("%d templates over %d pages of %d, each once with uid and create_time", len(listed), pages+1, listTemplatesPage), nil
}

// deleteTemplate: DeleteActorTemplate of a template an actor exists of is refused with FailedPrecondition and leaves
// the template (and its golden tag) in place; once the actor is gone the delete goes through, and the template and its
// golden tag are gone (Substrate deletes a template together with its golden actor and golden tag). The template is
// the golden-template case's, with its actor; without one, a copy of the suite's template with golden snapshots
// DISABLED and an actor of it.
func (r *run) deleteTemplate(ctx context.Context) (string, error) {
	name, actor, tag := r.golden.template, r.golden.actor, r.golden.tag
	if name == "" {
		name, actor = r.name("deltpl"), r.name("of-deltpl")
		tpl := proto.CloneOf(r.tpl)
		tpl.Metadata = &ateapi.ResourceMetadata{Atespace: r.cfg.Atespace, Name: name}
		tpl.Status = nil
		if tpl.SnapshotConfig == nil {
			tpl.SnapshotConfig = &ateapi.SnapshotConfig{}
		}
		tpl.SnapshotConfig.GoldenSnapshotConfig = &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}
		if _, err := r.c.CreateActorTemplate(ctx, tpl); err != nil {
			return "", gate("CreateActorTemplate "+name, err)
		}
		r.templates = append(r.templates, name)
		if _, err := r.newActorOf(ctx, name, actor, nil, ""); err != nil {
			return "", err
		}
	}
	_, err := r.c.DeleteActorTemplate(ctx, r.cfg.Atespace, name)
	if status.Code(err) == codes.Unimplemented {
		return "", gate("DeleteActorTemplate", err)
	}
	switch status.Code(err) {
	case codes.FailedPrecondition:
	case codes.OK:
		return "", fmt.Errorf("DeleteActorTemplate %s deleted it while actor %s of it exists", name, actor)
	default:
		return "", fmt.Errorf("DeleteActorTemplate %s with actor %s of it: %v, want FailedPrecondition", name, actor, err)
	}
	if _, err := r.c.GetActorTemplate(ctx, r.cfg.Atespace, name); err != nil {
		return "", fmt.Errorf("GetActorTemplate %s after a refused delete: %w", name, err)
	}
	if tag != nil {
		if _, err := r.c.GetTag(ctx, tag.GetAtespace(), tag.GetName()); err != nil {
			return "", fmt.Errorf("GetTag %s (the golden tag) after a refused delete: %w", tag.GetName(), err)
		}
	}
	if _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, actor); err != nil {
		return "", fmt.Errorf("DeleteActor %s: %w", actor, err)
	}
	if err := r.waitGone(ctx, actor); err != nil {
		return "", err
	}
	// The actor's delete may still be converging on the target.
	wctx, cancel := context.WithTimeout(ctx, r.cfg.Wait)
	defer cancel()
	for {
		_, err = r.c.DeleteActorTemplate(wctx, r.cfg.Atespace, name)
		if c := status.Code(err); c != codes.FailedPrecondition && c != codes.Unavailable {
			break
		}
		select {
		case <-wctx.Done():
			return "", fmt.Errorf("DeleteActorTemplate %s once actor %s is gone: %v (for %v)", name, actor, err, r.cfg.Wait)
		case <-time.After(r.cfg.Poll):
		}
	}
	if err != nil {
		return "", fmt.Errorf("DeleteActorTemplate %s once actor %s is gone: %w", name, actor, err)
	}
	if _, err := r.c.GetActorTemplate(ctx, r.cfg.Atespace, name); status.Code(err) != codes.NotFound {
		return "", fmt.Errorf("GetActorTemplate %s after its delete: %v, want NotFound", name, err)
	}
	gone := "no golden tag"
	if tag != nil {
		if _, err := r.c.GetTag(ctx, tag.GetAtespace(), tag.GetName()); status.Code(err) != codes.NotFound {
			return "", fmt.Errorf("GetTag %s (the golden tag) after the template's delete: %v, want NotFound", tag.GetName(), err)
		}
		gone = "golden tag " + tag.GetName() + " gone"
	}
	return fmt.Sprintf("refused with actor %s (FailedPrecondition); deleted once it was gone, %s", actor, gone), nil
}
