package handlers

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

// Log 只會在logger.go中呼叫
var Log zerolog.Logger

// // Level defines log levels.
// type Level uint8

// const (
// 	// DebugLevel defines debug log level.
// 	DebugLevel Level = iota
// 	// InfoLevel defines info log level.
// 	InfoLevel
// 	// WarnLevel defines warn log level.
// 	WarnLevel
// 	// ErrorLevel defines error log level.
// 	ErrorLevel
// 	// FatalLevel defines fatal log level.
// 	FatalLevel
// 	// PanicLevel defines panic log level.
// 	PanicLevel
// 	// NoLevel defines an absent log level.
// 	NoLevel
// 	// Disabled disables the logger.
// 	Disabled
// )

func init() {
	// set log level
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	// log format
	// Note: By default log writes to os.Stderr Note: The default log level for log.Print is debug
	output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}

	Log = zerolog.New(output).With().Timestamp().Logger()

	// SetLevel(DebugLevel) // set global level
}

// func SetLevel(l Level) {
// 	zerolog.SetGlobalLevel(zerolog.Level(l))
// }

func Infof(format string, v ...interface{}) {
	Log.Info().Msgf(format, v...)
}

func Debugf(format string, v ...interface{}) {
	Log.Debug().Msgf(format, v...)
}

func Warnf(format string, v ...interface{}) {
	Log.Warn().Msgf(format, v...)
}

func Errorf(format string, v ...interface{}) {
	Log.Error().Msgf(format, v...)
}

func Panicf(format string, v ...interface{}) {
	Log.Panic().Msgf(format, v...)
}
