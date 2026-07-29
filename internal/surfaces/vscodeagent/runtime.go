package vscodeagent

import (
	"context"
	"errors"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/localtools"
)

const Identity = "vscode"

var ErrSetupRequired = errors.New("vscodeagent: setup required")

type RuntimeHooks struct {
	AskApproval localtools.AskApproval
	EventSink   taskengine.TaskEventSink
}

type Runtime struct {
	Engine       *enginesvc.Engine
	Agent        agentservice.Agent
	Chain        *taskengine.TaskChainDefinition
	FIMChain     *taskengine.TaskChainDefinition
	CompactChain *taskengine.TaskChainDefinition
	Close        func()
}

type RuntimeBuilder func(ctx context.Context, hooks RuntimeHooks) (*Runtime, error)

func (r *Runtime) stop() {
	if r == nil {
		return
	}
	if r.Close != nil {
		r.Close()
		return
	}
	if r.Engine != nil && r.Engine.Stop != nil {
		r.Engine.Stop()
	}
}
