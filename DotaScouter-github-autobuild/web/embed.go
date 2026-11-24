package web

import "embed"

// StaticFS embeds the complete dark-themed HTML/CSS/JS dashboard into DotaScouter.exe.
//
//go:embed static/*
var StaticFS embed.FS
