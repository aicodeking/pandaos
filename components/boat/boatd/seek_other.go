//go:build !linux

package main

const seekData, seekHole = 4, 3 // darwin/BSD values
