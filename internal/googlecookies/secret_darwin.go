//go:build darwin

package googlecookies

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"
)

// NativeSupported reports whether this build can refresh cookies without an
// external script: macOS with a readable Chrome profile present. The first
// keychain read shows a one-time "openmessage wants to access Chrome Safe
// Storage" prompt; Always Allow persists it — scoped to this binary, not to the
// /usr/bin/security tool.
func NativeSupported() bool {
	profile := DefaultChromeProfile()
	if profile == "" {
		return false
	}
	for _, c := range []string{
		filepath.Join(profile, "Network", "Cookies"),
		filepath.Join(profile, "Cookies"),
	} {
		if _, err := os.Stat(c); err == nil {
			return true
		}
	}
	return false
}

func defaultChromeProfileDir(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "Default")
}

const chromeSafeStorageService = "Chrome Safe Storage"

// chromeSafeStorageSecret reads Chrome's cookie-encryption password from the
// login keychain via the Security framework, in-process. This is deliberately
// NOT shelled out to /usr/bin/security: the keychain ACL identifies the
// requesting application, so an in-process read prompts for (and "Always Allow"
// grants access to) the openmessage binary alone. Shelling out to `security`
// would instead add that general-purpose CLI to the item's ACL, letting any
// later `security find-generic-password` call read the key with no prompt.
//
// The value itself is never logged. ctx is accepted for signature
// compatibility; the keychain call is synchronous and blocks on the GUI prompt,
// which ctx cannot cancel.
func chromeSafeStorageSecret(_ context.Context) ([]byte, error) {
	service := C.CString(chromeSafeStorageService)
	defer C.free(unsafe.Pointer(service))

	var passwordLen C.UInt32
	var password unsafe.Pointer
	var searchList C.CFTypeRef // typed nil: search the default (login) keychain list

	status := C.SecKeychainFindGenericPassword(
		searchList,
		C.UInt32(len(chromeSafeStorageService)), service,
		0, nil, // no account name — match Chrome's single "Chrome Safe Storage" item
		&passwordLen, &password,
		nil, // no item ref needed
	)
	switch status {
	case C.errSecSuccess:
		// fall through
	case C.errSecItemNotFound:
		return nil, fmt.Errorf("Chrome Safe Storage key not found in keychain (is Chrome installed and run at least once?)")
	case C.errSecAuthFailed, C.errSecUserCanceled:
		return nil, fmt.Errorf("keychain access to Chrome Safe Storage was denied")
	default:
		return nil, fmt.Errorf("read Chrome Safe Storage from keychain: OSStatus %d", int(status))
	}
	defer C.SecKeychainItemFreeContent(nil, password)

	// C.GoBytes copies into Go-managed memory, so the result stays valid after
	// SecKeychainItemFreeContent releases the C buffer.
	secret := C.GoBytes(password, C.int(passwordLen))
	if len(secret) == 0 {
		return nil, fmt.Errorf("Chrome Safe Storage key was empty")
	}
	return secret, nil
}
