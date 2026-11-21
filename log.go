package main

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// newLogger builds the process-wide zerolog.Logger using ConsoleWriter: a coloured,
// human-readable line ("TIME LEVEL message key=value ...") rather than a raw structured
// dump. The level comes from SPOR_LOG_LEVEL so verbosity can be turned up without a rebuild.
//
// Call sites build a complete, readable sentence (fmt.Sprintf) and pass it as the message
// with no extra fields. Fields that ARE attached (a startup config dump, "component") are
// appended as trailing key=value pairs by ConsoleWriter; they are not the primary way
// information is conveyed.
func newLogger(levelStr string) zerolog.Logger {
	// "" means "use the default" (Info). ParseLevel("") returns NoLevel with a nil error,
	// so it has to be handled before the error branch below.
	level := zerolog.InfoLevel
	if levelStr != "" {
		if l, err := zerolog.ParseLevel(strings.ToLower(levelStr)); err == nil {
			level = l
		}
	}

	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
		// NoColor is deliberately left false: stdout is a pipe in a container, but
		// `docker logs` and the log viewers used here render ANSI colour fine.
	}

	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}

// withComponent returns a sub-logger tagging every line with a "component" field, which
// ConsoleWriter renders as a trailing component=x: "which subsystem logged this".
func withComponent(l zerolog.Logger, component string) zerolog.Logger {
	return l.With().Str("component", component).Logger()
}
