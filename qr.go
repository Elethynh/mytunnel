package main

import (
	"io"
	"os"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	terminalQRContrast = "\x1b[30;47m"
	terminalReset      = "\x1b[0m"
)

func encodeQRCode(address string, terminal bool) (string, error) {
	code, err := qrcode.New(address, qrcode.Medium)
	if err != nil {
		return "", err
	}
	text := code.ToSmallString(true)
	if terminal {
		return terminalQRContrast + text + terminalReset, nil
	}
	return text, nil
}

func writerIsTerminal(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
