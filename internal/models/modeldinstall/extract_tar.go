package modeldinstall

import (
	"compress/gzip"
	"os"

	"github.com/contenox/contenox/libarchiveutil"
)

// extractTarGz unpacks a .tar.gz archive into dest, rejecting any entry that
// would escape dest (absolute paths, `..`, escaping sym/hardlinks). Regular
// file modes are preserved so the launcher stays executable. Symlinks are
// supported (the native libs use them) but validated to stay within dest.
func extractTarGz(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return archiveutil.ExtractTar(gz, dest)
}
