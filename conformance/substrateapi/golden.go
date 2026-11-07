package substrateapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
)

// goldenTemplate: a template created with golden_snapshot_config.mode ENABLED gets a golden snapshot, reported as
// status.golden_snapshot_status.golden_tag within GoldenWait (or the build's error_message, which fails the case);
// an actor created from the template starts (from that snapshot, Substrate's contract) and runs a command. The
// template is a copy of the suite's own under a new name.
func (r *run) goldenTemplate(ctx context.Context) (string, error) {
	if r.cfg.NoGolden != "" {
		return "", notConfigured{"the target does not serve golden snapshots: " + r.cfg.NoGolden}
	}
	name := r.name("golden")
	tpl := proto.CloneOf(r.tpl)
	tpl.Metadata = &ateapi.ResourceMetadata{Atespace: r.cfg.Atespace, Name: name}
	tpl.Status = nil
	if tpl.SnapshotConfig == nil {
		tpl.SnapshotConfig = &ateapi.SnapshotConfig{}
	}
	tpl.SnapshotConfig.GoldenSnapshotConfig = &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED}
	if _, err := r.c.CreateActorTemplate(ctx, tpl); err != nil {
		return "", gate("CreateActorTemplate "+name, err)
	}
	r.templates = append(r.templates, name)
	bound := r.cfg.GoldenWait
	if bound == 0 {
		bound = r.cfg.Wait
	}
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var gs *ateapi.GoldenSnapshotStatus
	var lastErr error
	for gs.GetGoldenTag() == nil {
		got, err := r.c.GetActorTemplate(wctx, r.cfg.Atespace, name)
		if err == nil {
			gs, lastErr = got.GetStatus().GetGoldenSnapshotStatus(), nil
			if m := gs.GetErrorMessage(); m != "" {
				return "", fmt.Errorf("template %s: golden snapshot build failed: %s", name, m)
			}
			if gs.GetGoldenTag() != nil {
				break
			}
		} else {
			lastErr = err
		}
		select {
		case <-wctx.Done():
			return "", fmt.Errorf("template %s: no status.golden_snapshot_status.golden_tag within %v (last status: %v, %v)", name, bound, gs, lastErr)
		case <-time.After(r.cfg.Poll):
		}
	}
	built := time.Since(start).Round(time.Millisecond)
	tag := gs.GetGoldenTag()
	if t, err := r.c.GetTag(ctx, tag.GetAtespace(), tag.GetName()); err != nil {
		return "", fmt.Errorf("GetTag %s/%s (the golden tag): %w", tag.GetAtespace(), tag.GetName(), err)
	} else if t.GetStatus().GetSnapshot().GetDurableSnapshot().GetState() != ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
		return "", fmt.Errorf("golden tag %s/%s: snapshot %v, want COMPLETED", tag.GetAtespace(), tag.GetName(), t.GetStatus().GetSnapshot())
	}
	actor := r.name("from-golden")
	if _, err := r.newActorOf(ctx, name, actor, nil, ""); err != nil {
		return "", err
	}
	if _, err := r.resumeRunning(ctx, actor); err != nil {
		return "", err
	}
	ran := r.cfg.Probe.Name() + " answered"
	if ep, ok := r.cfg.Probe.(ExecProbe); ok {
		want := "golden-" + r.cfg.Prefix
		res, err := ep.Exec(ctx, r.cfg.Router, r.target(actor), "echo", want)
		if err != nil {
			return "", fmt.Errorf("exec in %s: %w", actor, err)
		}
		if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != want {
			return "", fmt.Errorf("exec echo %s in %s: exit %d, stdout %q, stderr %q", want, actor, res.ExitCode, res.Stdout, res.Stderr)
		}
		ran = "exec answered"
	} else if err := r.cfg.Probe.Request(ctx, r.cfg.Router, r.target(actor)); err != nil {
		return "", fmt.Errorf("%s in %s: %w", r.cfg.Probe.Name(), actor, err)
	}
	r.golden.template, r.golden.actor, r.golden.tag = name, actor, tag
	return fmt.Sprintf("golden tag %s after %v; actor %s from it RUNNING, %s", tag.GetName(), built, actor, ran), nil
}
