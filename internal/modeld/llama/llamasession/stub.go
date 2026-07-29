//go:build !llamanode || !llamacpp_direct

package llamasession

import (
	"errors"

	"github.com/contenox/contenox/internal/modeld/llama"
)

const Available = false

func New(_ string, _ llama.Config) (llama.Session, error) {
	return nil, errors.New("llamasession: built without the 'llamanode llamacpp_direct' tags")
}
