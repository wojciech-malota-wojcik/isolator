package wire

// Mount defines directories to mount inside container
type Mount struct {
	// Location on Host
	Host string

	// Mountpoint inside container
	Container string

	// Writable makes mount writable inside container
	Writable bool
}

// Config stores configuration of executor
type Config struct {
	// Chroot tells if chroot should be used instead of pivoting
	Chroot bool

	// Mounts is the list of bindings to apply inside container
	Mounts []Mount
}

// Execute is sent to execute a shell command
type Execute struct {
	// Command is a command to execute
	Command string
}

// Copy copies rsc to dst
type Copy struct {
	// Src points to source location
	Src string

	// Dst points to destination location
	Dst string
}

// Result is sent once command finishes
type Result struct {
	// Error is the error returned by command
	Error string
}

// Stream is the type of stream where log was produced
type Stream int

const (
	// StreamOut represents stdout
	StreamOut Stream = iota

	// StreamErr represents stderr
	StreamErr
)

// Log is the log message printed by executed command
type Log struct {
	// Stream is the type of stream where log was produced
	Stream Stream

	// Text is text printed by command
	Text string
}
