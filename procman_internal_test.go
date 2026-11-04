package procman

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestDroppedEventWarningUsesStructuredAttributes(t *testing.T) {
	pm := NewProcman()
	var logs bytes.Buffer
	pm.logger = slog.New(slog.NewTextHandler(&logs, nil))

	// Park the event loop on the first event so the queue fills behind it.
	release := make(chan struct{})
	pm.OnStart = func(*ExecCmd) { <-release }
	cmd := &ExecCmd{Name: "worker", procmanId: NewID()}
	for range cap(pm.evtCh) + 2 {
		pm.notifyOnStart(cmd)
	}
	close(release)
	pm.WaitEventLoop()
	pm.StopEventLoop()

	got := logs.String()
	if !strings.Contains(got, "name=worker") || !strings.Contains(got, "id="+cmd.ID().String()) {
		t.Errorf("warning lacks name/id attributes:\n%s", got)
	}
	if strings.Contains(got, "%s") || strings.Contains(got, "%v") {
		t.Errorf("warning contains an unformatted verb:\n%s", got)
	}
}
