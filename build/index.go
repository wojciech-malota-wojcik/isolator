package build

import "github.com/wojciech-malota-wojcik/buildgo"

// Commands is a definition of commands available in build system
var Commands = map[string]interface{}{
	"tools/build": buildMe,
	"build":       buildApp,
}

func init() {
	buildgo.AddCommands(Commands)
}
