package main

import (
	"github.com/wojciech-malota-wojcik/isolator/executor"
	"github.com/wojciech-malota-wojcik/run"
)

func main() {
	run.Service("executor", nil, executor.Run)
}
