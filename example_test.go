package procman_test

import (
	"context"
	"fmt"
	"time"

	"github.com/mainvec/procman"
)

// Example_supervise shows a minimal supervisor that starts a process, waits
// for it to exit, and prints the exit info and the retained log tail. It
// mirrors the Quick Start snippet in the README.
func Example_supervise() {
	s := procman.New(procman.Options{Watchdog: procman.WatchdogAuto})
	defer s.Close()

	// Use the test child as a stand-in for a real server.
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "server",
		Path:      exampleChildPath(),
		Args:      procman.TestChildArgs(0, 100*time.Millisecond),
		Env:       procman.TestChildEnv(),
		StopGrace: 2 * time.Second,
		LogTailLines: 10,
	})
	if err != nil {
		fmt.Printf("start: %v\n", err)
		return
	}

	<-p.Done()
	info, _ := p.Exit()
	fmt.Printf("server exited: code=%d\n", info.Code)
}

// exampleChildPath returns the test binary path for the example, or a
// placeholder if it is unavailable. The example is purely illustrative.
func exampleChildPath() string {
	exe, err := procman.TestChildExe()
	if err != nil {
		return "/bin/true"
	}
	return exe
}