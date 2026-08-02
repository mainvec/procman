package procman

import "os/exec"

// prepareChildCmd applies platform-specific SysProcAttr to the command before
// Start. For T3 this is a no-op; process-group isolation arrives in T7 (Unix)
// and the Job Object in T8 (Windows).
func prepareChildCmd(cmd *exec.Cmd) {}