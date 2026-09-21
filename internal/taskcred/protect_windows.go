package taskcred

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows file mode bits do not express ACL privacy. DPAPI instead binds
// credential contents to the current Windows user.
func checkPrivate(os.FileInfo) error       { return nil }
func protect(raw []byte) ([]byte, error)   { return crypt(raw, true) }
func unprotect(raw []byte) ([]byte, error) { return crypt(raw, false) }
func crypt(raw []byte, encrypt bool) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty protected credential")
	}
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	var err error
	if encrypt {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, int(out.Size))...), nil
}
