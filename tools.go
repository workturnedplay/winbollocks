//go:build tools

// This file exists solely to ensure that development and code-generation
// dependencies (like winres for building the multi-res .ico and embedding the
// .syso resource file) are tracked by 'go mod tidy' and included in 'vendor/',
// while remaining completely ignored by standard 'go build' commands.

package main

import (
	_ "github.com/tc-hib/winres"
)
