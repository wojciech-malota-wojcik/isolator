package wire

// SocketPath is the path to unix socket file
const SocketPath = ".executor.sock"

// Execute is sent to execute a shell command
type Execute struct {

	// Command is a command to execute
	Command string
}

// Completed is sent once command finishes
type Completed struct {
	// ExitCode is the exit code of command
	ExitCode int

	// Error is the error returned by command
	Error string
}
