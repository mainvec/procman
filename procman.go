package procman

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

var (
	ErrExecCmdNotStarted     = errors.New("child process not started")
	ErrExecCmdAlreadyStarted = errors.New("child process already started")
)

type ExecCmdStatus int

const (
	ExecCmdStatusNotStarted = iota
	ExecCmdStatusRunning
	ExecCmdStatusExited
	ExecCmdStatusFailed
)

type Procman struct {
	execCmds map[ID]*ExecCmd
}

type RestartPolicy int

const (
	RestartPolicyNever RestartPolicy = iota
	RestartPolicyAlways
	RestartPolicyOnFailure
)

type ExecCmd struct {
	Name          string
	Args          []string
	procman       *Procman
	procmanId     ID
	pid           int
	ctx           context.Context
	cancel        context.CancelFunc
	cmd           *exec.Cmd
	started       bool
	startedAt     time.Time
	status        int
	lastError     error
	waitChan      chan error
	restartPolicy RestartPolicy
}

func NewProcman() *Procman {

	return &Procman{
		execCmds: make(map[ID]*ExecCmd),
	}
}

func (p *Procman) NewExecCmd(name string, args ...string) *ExecCmd {
	childId := NewID()
	child := &ExecCmd{
		Name:      name,
		Args:      args,
		procman:   p,
		procmanId: childId,
		status:    ExecCmdStatusNotStarted,
		waitChan:  make(chan error, 1),
	}
	//should check
	p.execCmds[childId] = child
	return child
}

func (p *Procman) GetExecCmd(id ID) (*ExecCmd, bool) {
	child, ok := p.execCmds[id]
	return child, ok
}

func (p *Procman) RemoveExecCmd(id ID) {
	delete(p.execCmds, id)
}

func (p *Procman) ListExecCmdes() []*ExecCmd {
	children := make([]*ExecCmd, 0, len(p.execCmds))
	for _, child := range p.execCmds {
		children = append(children, child)
	}
	return children
}

func (p *Procman) KillAllExecCmdes() {
	for _, child := range p.execCmds {
		if child.started {
			child.cmd.Process.Kill()
		}
	}
}

func (c *ExecCmd) Start() error {
	if c.started {
		return ErrExecCmdAlreadyStarted
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel
	cmd := exec.CommandContext(c.ctx, c.Name, c.Args...)
	c.cmd = cmd
	prepareChildCmd(cmd)
	err := cmd.Start()
	if err != nil {
		c.status = ExecCmdStatusFailed
		return err
	}
	c.started = true
	c.status = ExecCmdStatusRunning
	go func() {
		err := cmd.Wait()
		if err != nil {
			c.status = ExecCmdStatusFailed
			c.lastError = err
		} else {
			c.status = ExecCmdStatusExited
		}
		c.waitChan <- err
	}()
	c.pid = cmd.Process.Pid
	return nil

}

func (c *ExecCmd) Wait() error {
	if c.cmd == nil || !c.started {
		return ErrExecCmdNotStarted
	}
	err := <-c.waitChan
	return err
}

func (c *ExecCmd) GetProcessState() *os.ProcessState {
	if c.cmd == nil || c.cmd.Process == nil || !c.started {
		return nil
	}
	return c.cmd.ProcessState
}

/*



	ctx, cencel := context.WithCancel(context.Background())
	defer cencel()
	cmd := exec.CommandContext(ctx, "ls", "-WAQ")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Start()
	go io.Copy(os.Stdout, stdout)
	go io.Copy(os.Stderr, stderr)

	err = cmd.Wait()

	if err != nil {
		t.Fatal(err)
	}
}

*/
