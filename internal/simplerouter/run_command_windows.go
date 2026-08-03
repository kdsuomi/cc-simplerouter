//go:build windows

package simplerouter

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSetConsoleCtrlHandler = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")

func runCodexCommand(spec launchSpec) error {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	job, _ := newKillOnCloseJob()
	if job != 0 {
		assignProcessToJob(job, uint32(cmd.Process.Pid))
	}

	var closeJobOnce sync.Once
	closeJob := func() {
		if job != 0 {
			closeJobOnce.Do(func() { _ = windows.CloseHandle(job) })
		}
	}
	defer closeJob()

	removeHandler := installConsoleCloseHandler(func() {
		_ = cmd.Process.Kill()
		closeJob()
		os.Exit(1)
	})
	defer removeHandler()

	return cmd.Wait()
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func assignProcessToJob(job windows.Handle, pid uint32) {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(process)
	_ = windows.AssignProcessToJobObject(job, process)
}

func installConsoleCloseHandler(onClose func()) func() {
	callback := syscall.NewCallback(func(ctrlType uint32) uintptr {
		switch ctrlType {
		case windows.CTRL_CLOSE_EVENT, windows.CTRL_LOGOFF_EVENT, windows.CTRL_SHUTDOWN_EVENT:
			onClose()
			return 1
		default:
			return 0
		}
	})

	r1, _, _ := procSetConsoleCtrlHandler.Call(callback, 1)
	if r1 == 0 {
		return func() {}
	}
	return func() {
		procSetConsoleCtrlHandler.Call(callback, 0)
	}
}
