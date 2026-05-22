//go:build !linux

package gui

import (
	"fmt"
	"os"

	"github.com/secureNqwer/zerolink/messenger"
)

func Run(engine *messenger.Engine, _ interface{}) {
	fmt.Println("Desktop GUI is only supported on Linux (requires WebKit)")
	os.Exit(1)
}
