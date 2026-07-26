package tooleval

import (
	"os"
	"path/filepath"
)

// binaryProjectName is the trap: an executable at the workspace ROOT sharing the name
// of the real project (which lives under src/). This is yesterday's incident verbatim
// — the first fleet dispatch mistook a same-named 50 MB executable for the project.
const binaryProjectName = "widget"

// largeBinaryBytes is the size of the synthetic executable. 50 MiB matches the
// incident and is created sparse (a small binary header, then a truncate-to-size), so
// it is instant and cheap on disk while os.Stat/list_dir still report the full apparent
// size — enough to trip both the >1 MiB list_dir size suffix and read_file's read cap.
const largeBinaryBytes = 50 << 20 // 50 MiB

// binaryHeader is a short ELF-ish prefix carrying NUL bytes so the localtools binary
// sniff (a NUL in the first ~512 bytes) classifies the file as binary, exactly as a
// real compiled executable would.
var binaryHeader = append([]byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}, make([]byte, 120)...)

func init() {
	RegisterFixtureBuilder("binary-not-a-project", buildBinaryNotAProjectFixture)
}

// buildBinaryNotAProjectFixture writes the hostile shape the committed tree cannot
// hold: a ~50 MiB executable named like the project, at the workspace root, with the
// executable bit set and a binary (NUL-bearing) header. The real project text lives in
// the static fixture/ tree under src/<project>/.
func buildBinaryNotAProjectFixture(ws string) error {
	path := filepath.Join(ws, binaryProjectName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := f.Write(binaryHeader); err != nil {
		f.Close()
		return err
	}
	// Extend to the full apparent size sparsely — no 50 MiB of real bytes written.
	if err := f.Truncate(largeBinaryBytes); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Truncate can clear the exec bits on some platforms; re-assert them.
	return os.Chmod(path, 0o755)
}
