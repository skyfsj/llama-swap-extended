//go:build windows

package modeldownload

// fileIsSparse cannot be determined portably on Windows through os.FileInfo;
// report false and let the checksum (when the provider advertises one) be the
// integrity gate.
func fileIsSparse(string) (bool, error) {
	return false, nil
}
