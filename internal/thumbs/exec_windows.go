package thumbs

import (
	"os/exec"
	"syscall"
)

// hideWindow stops each ffmpeg run from flashing a console window on the
// desktop. 0x08000000 is CREATE_NO_WINDOW.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
