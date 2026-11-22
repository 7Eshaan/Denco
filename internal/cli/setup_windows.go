//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

// registryPath edits the user's PATH: HKCU\Environment\Path, the setting
// behind "Edit environment variables for your account". No administrator
// rights needed. Its type (REG_EXPAND_SZ, usually) and other entries are
// kept as they are.
type registryPath struct{}

var (
	advapi32               = syscall.NewLazyDLL("advapi32.dll")
	procRegSetValueExW     = advapi32.NewProc("RegSetValueExW")
	user32                 = syscall.NewLazyDLL("user32.dll")
	procSendMessageTimeout = user32.NewProc("SendMessageTimeoutW")
)

const errFileNotFound = syscall.Errno(2)

func openEnvKey(access uint32) (syscall.Handle, error) {
	var h syscall.Handle
	name, _ := syscall.UTF16PtrFromString("Environment")
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, name, 0, access, &h); err != nil {
		return 0, fmt.Errorf("opening HKCU\\Environment: %w", err)
	}
	return h, nil
}

// readUserPath returns the user PATH and its registry type ("" and
// REG_EXPAND_SZ when there is none yet).
func readUserPath() (string, uint32, error) {
	h, err := openEnvKey(syscall.KEY_QUERY_VALUE)
	if err != nil {
		return "", 0, err
	}
	defer syscall.RegCloseKey(h)
	name, _ := syscall.UTF16PtrFromString("Path")
	var typ, size uint32
	err = syscall.RegQueryValueEx(h, name, nil, &typ, nil, &size)
	if errors.Is(err, errFileNotFound) {
		return "", syscall.REG_EXPAND_SZ, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("reading the user PATH: %w", err)
	}
	if typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ {
		return "", 0, fmt.Errorf("the user PATH has an unexpected registry type %d", typ)
	}
	buf := make([]uint16, size/2+1)
	if err := syscall.RegQueryValueEx(h, name, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil {
		return "", 0, fmt.Errorf("reading the user PATH: %w", err)
	}
	return syscall.UTF16ToString(buf), typ, nil
}

func writeUserPath(value string, typ uint32) error {
	h, err := openEnvKey(syscall.KEY_SET_VALUE)
	if err != nil {
		return err
	}
	defer syscall.RegCloseKey(h)
	name, _ := syscall.UTF16PtrFromString("Path")
	data, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	r, _, _ := procRegSetValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0, uintptr(typ),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	if r != 0 {
		return fmt.Errorf("writing the user PATH: %w", syscall.Errno(r))
	}
	// Tell Explorer (and so every terminal opened from now on) that the
	// environment changed; already-open terminals keep their old PATH.
	env, _ := syscall.UTF16PtrFromString("Environment")
	var result uintptr
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	procSendMessageTimeout.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)),
		smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
	return nil
}

func (registryPath) Has(dir string) (bool, error) {
	list, _, err := readUserPath()
	return pathListHas(list, dir), err
}

func (registryPath) Add(dir string) (string, error) {
	list, typ, err := readUserPath()
	if err != nil {
		return "", err
	}
	return "your user PATH", writeUserPath(pathListAdd(list, dir), typ)
}

func (registryPath) Remove(dir string) (bool, error) {
	list, typ, err := readUserPath()
	if err != nil {
		return false, err
	}
	rest, removed := pathListRemove(list, dir)
	if !removed {
		return false, nil
	}
	return true, writeUserPath(rest, typ)
}

func (registryPath) Reload() string { return "" }

// deleteAfterExit removes the running denco.exe (and its folder, if
// empty) a moment after this process exits: Windows won't delete a
// running program. A detached cmd.exe waits ~2 s, then deletes.
func deleteAfterExit(path, dir string) error {
	cmd := exec.Command("cmd.exe")
	detach(cmd)
	cmd.SysProcAttr.CmdLine = fmt.Sprintf(`cmd.exe /d /c ping -n 3 127.0.0.1 >nul & del /f /q "%s" & del /f /q "%s.old" & rmdir "%s"`, path, path, dir)
	return cmd.Start()
}
