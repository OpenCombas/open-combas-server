package main

import (
	"log"
	"os"
)

var (
	Info  = log.New(os.Stdout, "INFO: ", log.LstdFlags)
	Warn  = log.New(os.Stdout, "WARN: ", log.LstdFlags)
	Error = log.New(os.Stderr, "ERROR: ", log.LstdFlags)
)
