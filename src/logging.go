package main

import (
	"log"
	"os"
)

type Color string

const (
	ColorReset  Color = "\033[0m"
	ColorRed    Color = "\033[31m"
	ColorYellow Color = "\033[33m"
	ColorGreen  Color = "\033[32m"
)

var (
	Info  = log.New(os.Stdout, wrapInColor("INFO : ", ColorGreen), log.LstdFlags)
	Warn  = log.New(os.Stdout, wrapInColor("WARN : ", ColorYellow), log.LstdFlags)
	Error = log.New(os.Stderr, wrapInColor("ERROR: ", ColorRed), log.LstdFlags)
)

func wrapInColor(label string, color Color) string {
	return string(color) + label + string(ColorReset)
}
