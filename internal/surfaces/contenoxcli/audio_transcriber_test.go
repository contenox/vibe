package contenoxcli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/llmresolver"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

type stubModelRepo struct {
	gotMessages []libmodelprovider.Message
	reply       string
	err         error
}

func (s *stubModelRepo) Chat(_ context.Context, _ llmrepo.Request, messages []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
	s.gotMessages = messages
	if s.err != nil {
		return libmodelprovider.ChatResult{}, llmrepo.Meta{}, s.err
	}
	return libmodelprovider.ChatResult{Message: libmodelprovider.Message{Role: "assistant", Content: s.reply}}, llmrepo.Meta{}, nil
}

func (s *stubModelRepo) Tokenize(context.Context, string, string) ([]int, error) {
	return nil, errors.New("stub: not implemented")
}

func (s *stubModelRepo) CountTokens(context.Context, string, string) (int, error) {
	return 0, errors.New("stub: not implemented")
}

func (s *stubModelRepo) PromptExecute(context.Context, llmrepo.Request, string, float32, string) (string, llmrepo.Meta, error) {
	return "", llmrepo.Meta{}, errors.New("stub: not implemented")
}

func (s *stubModelRepo) Embed(context.Context, llmrepo.EmbedRequest, string) ([]float64, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("stub: not implemented")
}

func (s *stubModelRepo) Stream(context.Context, llmrepo.Request, []libmodelprovider.Message, ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("stub: not implemented")
}

var _ llmrepo.ModelRepo = (*stubModelRepo)(nil)

func wavFixture(t *testing.T, samples int) []byte {
	t.Helper()
	data := make([]byte, samples*2)
	var b bytes.Buffer
	b.WriteString("RIFF")
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(36+len(data))))
	b.WriteString("WAVEfmt ")
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(16)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint16(1)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint16(1)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(8000)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(16000)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint16(2)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint16(16)))
	b.WriteString("data")
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(len(data))))
	b.Write(data)
	return b.Bytes()
}

func audioToolsetFixture(t *testing.T, root string) map[string]taskengine.ToolsRepo {
	t.Helper()
	gt, err := gojatool.New(gojatool.Config{})
	require.NoError(t, err)
	t.Cleanup(gt.Shutdown)
	return localToolset(chatOpts{EffectiveLocalExecAllowedDir: root}, nil, libtracker.NoopTracker{}, gt, missionservice.New(nil))
}

func execReadFile(t *testing.T, tools map[string]taskengine.ToolsRepo, path string) (any, error) {
	t.Helper()
	res, _, err := tools["local_fs"].Exec(context.Background(), time.Now(),
		map[string]any{"path": path}, false, &taskengine.ToolsCall{Name: "local_fs", ToolName: "read_file"})
	return res, err
}

// TestUnit_BindAudioTranscriber_ReadFileReturnsTranscript asserts read_file on a wav answers with the model's transcript, the audio traveling as an AudioPart beside the fixed instruction in one user turn.
func TestUnit_BindAudioTranscriber_ReadFileReturnsTranscript(t *testing.T) {
	root := t.TempDir()
	wav := wavFixture(t, 64)
	require.NoError(t, os.WriteFile(filepath.Join(root, "memo.wav"), wav, 0o644))

	tools := audioToolsetFixture(t, root)
	stub := &stubModelRepo{reply: "meeting moved to thursday"}
	bindAudioTranscriber(tools, &enginesvc.Engine{Models: stub})

	res, err := execReadFile(t, tools, "memo.wav")
	require.NoError(t, err)
	out := res.(string)
	require.Contains(t, out, "meeting moved to thursday", "the transcript is the tool result")
	require.Contains(t, out, "memo.wav")
	require.Contains(t, out, "audio/wav")

	require.Len(t, stub.gotMessages, 1, "one user turn carries the audio")
	msg := stub.gotMessages[0]
	require.Equal(t, "user", msg.Role)
	require.Equal(t, transcribeInstruction, msg.Content, "the fixed instruction rides beside the attachment")
	require.Len(t, msg.Audio, 1)
	require.Equal(t, "audio/wav", msg.Audio[0].MimeType)
	require.Equal(t, wav, msg.Audio[0].Data, "the whole file reaches the model route")
}

// TestUnit_BindAudioTranscriber_NoAudioCapableModelSurfacesActionably asserts the resolver's shortfall plus the config key to set, aligned with the unbound-seam refusal.
func TestUnit_BindAudioTranscriber_NoAudioCapableModelSurfacesActionably(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "memo.wav"), wavFixture(t, 64), 0o644))

	tools := audioToolsetFixture(t, root)
	stub := &stubModelRepo{err: fmt.Errorf("%w: matching models [qwen3:8b] do not accept audio; use an audio-capable model for requests with audio attachments", llmresolver.ErrNoAudioCapableModel)}
	bindAudioTranscriber(tools, &enginesvc.Engine{Models: stub})

	_, err := execReadFile(t, tools, "memo.wav")
	require.Error(t, err)
	require.Contains(t, err.Error(), "transcribing memo.wav", "the tool names what it was doing")
	require.Contains(t, err.Error(), "no available model supports audio input", "the resolver's classification survives")
	require.Contains(t, err.Error(), "default-audio-model", "the fix is named, same key as the unbound-seam refusal")

	// Nil-safety of the bind itself: the setup-only posture passes no engine, and the toolset must keep refusing audio with the config key named.
	unbound := audioToolsetFixture(t, root)
	bindAudioTranscriber(unbound, nil)
	_, err = execReadFile(t, unbound, "memo.wav")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no audio model is configured")
	require.Contains(t, err.Error(), "default-audio-model")
}
