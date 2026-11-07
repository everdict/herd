package substrateapi

import (
	"context"
	"errors"
	"fmt"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// listTagsPage is the page size the list-tags case pages with: small, so a suite's few tags take several pages.
const listTagsPage = 2

// listTags: ListTags in the suite's atespace, paged to the end, names each tag once, each in that atespace with a uid,
// a create_time, a scope and status.actor_template_uid. Every listed tag GetTag still finds has GetTag's uid, scope,
// source actor and template uid; every tag the suite created that GetTag still finds is listed; and the
// golden-template case's golden tag is listed PUBLISHED under its template's uid. A target that does not serve it
// declares a skip.
func (r *run) listTags(ctx context.Context) (string, error) {
	listed := map[string]*ateapi.Tag{}
	token := ""
	pages := 0
	for ; ; pages++ {
		if pages == 5000 {
			return "", errors.New("ListTags: still a next_page_token after 5000 pages")
		}
		resp, err := r.c.ListTags(ctx, r.cfg.Atespace, &ateapi.ListOptions{PageSize: listTagsPage, PageToken: token})
		if err := gate("ListTags", err); err != nil {
			return "", err
		}
		if n := len(resp.GetTags()); n > listTagsPage {
			return "", fmt.Errorf("ListTags: a page of %d with page_size %d", n, listTagsPage)
		}
		for _, t := range resp.GetTags() {
			md := t.GetMetadata()
			switch {
			case md.GetName() == "":
				return "", fmt.Errorf("ListTags: a tag with no name: %v", md)
			case md.GetAtespace() != r.cfg.Atespace:
				return "", fmt.Errorf("ListTags %s: tag %s is in atespace %q", r.cfg.Atespace, md.GetName(), md.GetAtespace())
			case listed[md.GetName()] != nil:
				return "", fmt.Errorf("ListTags: tag %s listed twice", md.GetName())
			case md.GetUid() == "":
				return "", fmt.Errorf("ListTags: tag %s has no uid", md.GetName())
			case md.GetCreateTime() == nil || md.GetCreateTime().AsTime().IsZero():
				return "", fmt.Errorf("ListTags: tag %s has no create_time", md.GetName())
			case t.GetScope() == ateapi.TagScope_TAG_SCOPE_UNSPECIFIED:
				return "", fmt.Errorf("ListTags: tag %s has no scope", md.GetName())
			case t.GetStatus().GetActorTemplateUid() == "":
				return "", fmt.Errorf("ListTags: tag %s has no status.actor_template_uid", md.GetName())
			}
			listed[md.GetName()] = t
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	for _, n := range r.tags {
		if _, err := r.c.GetTag(ctx, r.cfg.Atespace, n); err == nil && listed[n] == nil {
			return "", fmt.Errorf("ListTags does not list %s, which the suite created", n)
		}
	}
	checked := 0
	for n, got := range listed {
		want, err := r.c.GetTag(ctx, r.cfg.Atespace, n)
		if status.Code(err) == codes.NotFound {
			continue // deleted since it was listed
		}
		if err != nil {
			return "", fmt.Errorf("GetTag %s: %w", n, err)
		}
		if got.GetScope() != want.GetScope() || !proto.Equal(got.GetSourceActor(), want.GetSourceActor()) ||
			got.GetStatus().GetActorTemplateUid() != want.GetStatus().GetActorTemplateUid() || got.GetMetadata().GetUid() != want.GetMetadata().GetUid() {
			return "", fmt.Errorf("ListTags lists %s as scope %v, source actor %v, template uid %q, uid %q; GetTag answers %v, %v, %q, %q", n,
				got.GetScope(), got.GetSourceActor(), got.GetStatus().GetActorTemplateUid(), got.GetMetadata().GetUid(),
				want.GetScope(), want.GetSourceActor(), want.GetStatus().GetActorTemplateUid(), want.GetMetadata().GetUid())
		}
		checked++
	}
	golden := ""
	if tag := r.golden.tag; tag != nil {
		tpl, err := r.c.GetActorTemplate(ctx, r.cfg.Atespace, r.golden.template)
		if err != nil {
			return "", fmt.Errorf("GetActorTemplate %s: %w", r.golden.template, err)
		}
		got := listed[tag.GetName()]
		if got == nil || got.GetScope() != ateapi.TagScope_TAG_SCOPE_PUBLISHED || got.GetStatus().GetActorTemplateUid() != tpl.GetMetadata().GetUid() {
			return "", fmt.Errorf("ListTags lists the golden tag %s as %v; want it PUBLISHED under template uid %s", tag.GetName(), got, tpl.GetMetadata().GetUid())
		}
		golden = ", the golden tag PUBLISHED under its template's uid"
	}
	return fmt.Sprintf("%d tags over %d pages of %d, each once with uid, create_time, scope and template uid; %d as GetTag answers them (scope, source actor, template uid)%s",
		len(listed), pages+1, listTagsPage, checked, golden), nil
}
