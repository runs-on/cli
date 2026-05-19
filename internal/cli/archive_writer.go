package cli

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

type archiveWriter struct {
	file   *os.File
	writer *zip.Writer
	closed bool
}

func newArchiveWriter(filePath string) (*archiveWriter, error) {
	file, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	return &archiveWriter{
		file:   file,
		writer: zip.NewWriter(file),
	}, nil
}

func (a *archiveWriter) writeJSON(entryPath string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return a.writeBytes(entryPath, append(data, '\n'))
}

func (a *archiveWriter) writeText(entryPath, text string) error {
	return a.writeBytes(entryPath, []byte(text))
}

func (a *archiveWriter) writeBytes(entryPath string, data []byte) error {
	entryName, err := archiveEntryName(entryPath)
	if err != nil {
		return err
	}
	fileWriter, err := a.writer.Create(entryName)
	if err != nil {
		return err
	}
	_, err = fileWriter.Write(data)
	return err
}

func (a *archiveWriter) writeFile(entryPath, filePath string) error {
	entryName, err := archiveEntryName(entryPath)
	if err != nil {
		return err
	}

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = entryName

	fileWriter, err := a.writer.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(fileWriter, file)
	return err
}

func (a *archiveWriter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	return errors.Join(a.writer.Close(), a.file.Close())
}

func archiveEntryName(entryPath string) (string, error) {
	if entryPath == "" {
		return "", fmt.Errorf("archive entry path is required")
	}

	entryName := strings.ReplaceAll(entryPath, "\\", "/")
	entryName = path.Clean(entryName)
	entryName = strings.TrimPrefix(entryName, "/")
	if entryName == "" || entryName == "." {
		return "", fmt.Errorf("archive entry path is required")
	}
	return entryName, nil
}
