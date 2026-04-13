package lima

import (
	"io"
	"os"
)

func execStdin() io.Reader  { return os.Stdin }
func execStdout() io.Writer { return os.Stdout }
func execStderr() io.Writer { return os.Stderr }
