package rpc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var errUnprotectedToken = errors.New("the API token file has a shared ACL; restart the API server to rotate it")

const tokenSecurity = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION

// LoadOrCreateToken opens a private token or rotates a token from an older
// version while holding exclusive access to the file.
func LoadOrCreateToken(path string) (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	token, err := readToken(path, user.User.Sid)
	if err == nil && token != "" {
		return token, nil
	}
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing && !errors.Is(err, errUnprotectedToken) {
		return "", err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return "", err
	}
	const access = windows.GENERIC_READ | windows.GENERIC_WRITE | windows.READ_CONTROL | windows.WRITE_DAC
	if missing {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
		file, err := openToken(path, access, 0, windows.CREATE_NEW, &sa)
		if err == nil {
			defer file.Close()
			if err := verifyToken(file, user.User.Sid); err != nil {
				return "", err
			}
			return writeToken(file)
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	file, err := openToken(path, access, 0, windows.OPEN_EXISTING, nil)
	if err != nil {
		return "", err
	}
	defer file.Close()
	err = verifyToken(file, user.User.Sid)
	unprotected := errors.Is(err, errUnprotectedToken)
	if err != nil && !unprotected {
		return "", err
	}
	token, err = tokenContents(file)
	if err != nil || (!unprotected && token != "") {
		return token, err
	}
	// The old app always generated a 32-byte hex token. Leave other files
	// untouched if a token path was entered incorrectly.
	if token != "" {
		raw, err := hex.DecodeString(token)
		if err != nil || len(raw) != tokenBytes {
			return "", errors.New("choose a new --token-file path for the API token")
		}
	}
	token, err = writeToken(file)
	if err != nil || !unprotected {
		return token, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return "", err
	}
	// Publish the new token only after it is private. A failed or interrupted
	// upgrade still leaves the old ACL, which clients refuse to trust.
	if err := windows.SetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return "", err
	}
	return token, verifyToken(file, user.User.Sid)
}

// ReadToken reads a token only when its owner and ACL belong to this user.
func ReadToken(path string) (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	token, err := readToken(path, user.User.Sid)
	if err == nil && token == "" {
		err = errors.New("the API token file is empty; restart the API server")
	}
	return token, err
}

func readToken(path string, sid *windows.SID) (string, error) {
	file, err := openToken(path, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ, windows.OPEN_EXISTING, nil)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := verifyToken(file, sid); err != nil {
		return "", err
	}
	return tokenContents(file)
}

func openToken(path string, access, share, disposition uint32, sa *windows.SecurityAttributes) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(absolute)
	if strings.Contains(absolute[len(volume):], ":") || len(volume) != 2 || volume[1] != ':' {
		return nil, errors.New("choose a local file path for the API token")
	}
	name, err := windows.UTF16PtrFromString(`\\?\` + absolute)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Second)
	for {
		handle, err := windows.CreateFile(name, access, share, sa, disposition, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err == nil {
			return os.NewFile(uintptr(handle), path), nil
		}
		if err != windows.ERROR_SHARING_VIOLATION || time.Now().After(deadline) {
			return nil, &os.PathError{Op: "open API token", Path: path, Err: err}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func verifyToken(file *os.File, sid *windows.SID) error {
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return errors.New("choose a regular, unlinked file for the API token")
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, tokenSecurity)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !owner.Equals(sid) {
		return errors.New("the API token file must be owned by the current Windows user")
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 || dacl == nil || dacl.AceCount != 1 {
		return errUnprotectedToken
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return err
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
		return errUnprotectedToken
	}
	return nil
}

func tokenContents(file *os.File) (string, error) {
	const limit = 4096
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && len(data) > limit {
		err = errors.New("the API token file is too large")
	}
	return strings.TrimSpace(string(data)), err
}

func writeToken(file *os.File) (string, error) {
	var bytes [tokenBytes]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes[:])
	if err := file.Truncate(0); err != nil {
		return "", err
	}
	if _, err := file.WriteAt([]byte(token+"\n"), 0); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("save API token: %w", err)
	}
	return token, nil
}
