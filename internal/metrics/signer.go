//go:build windows

package metrics

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	crypt32              = windows.NewLazySystemDLL("crypt32.dll")
	procCryptMsgGetParam = crypt32.NewProc("CryptMsgGetParam")
	procCryptMsgClose    = crypt32.NewProc("CryptMsgClose")
)

const cmsgSignerCertInfoParam = 7 // CMSG_SIGNER_CERT_INFO_PARAM

// SignerName returns the display name of the certificate that signed path
// (the embedded Authenticode signature's first signer). It does not check
// trust; pair it with WinVerifyTrust.
func SignerName(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var enc, ctype, ftype uint32
	var store, msg windows.Handle
	if err := windows.CryptQueryObject(windows.CERT_QUERY_OBJECT_FILE, unsafe.Pointer(p),
		windows.CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED_EMBED, windows.CERT_QUERY_FORMAT_FLAG_BINARY, 0,
		&enc, &ctype, &ftype, &store, &msg, nil); err != nil {
		return "", fmt.Errorf("read signature of %s: %w", path, err)
	}
	defer windows.CertCloseStore(store, 0)
	defer procCryptMsgClose.Call(uintptr(msg))

	var size uint32
	if r, _, e := procCryptMsgGetParam.Call(uintptr(msg), cmsgSignerCertInfoParam, 0, 0, uintptr(unsafe.Pointer(&size))); r == 0 {
		return "", fmt.Errorf("signer of %s: %w", path, e)
	}
	info := make([]byte, size)
	if r, _, e := procCryptMsgGetParam.Call(uintptr(msg), cmsgSignerCertInfoParam, 0, uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return "", fmt.Errorf("signer of %s: %w", path, e)
	}
	cert, err := windows.CertFindCertificateInStore(store, windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, 0,
		windows.CERT_FIND_SUBJECT_CERT, unsafe.Pointer(&info[0]), nil)
	if err != nil {
		return "", fmt.Errorf("signer certificate of %s: %w", path, err)
	}
	defer windows.CertFreeCertificateContext(cert)
	buf := make([]uint16, 256)
	n := windows.CertGetNameString(cert, windows.CERT_NAME_SIMPLE_DISPLAY_TYPE, 0, nil, &buf[0], uint32(len(buf)))
	if n <= 1 {
		return "", fmt.Errorf("signer of %s has no name", path)
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// signedByAMD reports whether the signer is Advanced Micro Devices.
func signedByAMD(path string) error {
	name, err := SignerName(path)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.ToLower(name), "advanced micro devices") {
		return fmt.Errorf("%s is signed by %q, not by AMD", path, name)
	}
	return nil
}
