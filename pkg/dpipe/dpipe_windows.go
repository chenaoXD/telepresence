package dpipe

import (
	"context"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/datawire/dlib/dexec"
)

func waitCloseAndKill(ctx context.Context, cmd *dexec.Cmd, peer io.Closer, closing *int32, killTimer **time.Timer) {
	<-ctx.Done()

	// A process is sometimes not terminated gracefully by the SIGTERM, so we give
	// it a second to succeed and then kill it forcefully.
	*killTimer = &time.Timer{} // Dummy timer since there's no correspondence to a hard kill
	atomic.StoreInt32(closing, 1)

	_ = peer.Close()
	// This kills the process and any child processes that it has started. Very important when
	// killing sshfs-win since it starts a cygwin sshfs process that must be killed along with it
	_ = dexec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
