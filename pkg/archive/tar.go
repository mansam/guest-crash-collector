package archive

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"time"
)

type Artifact struct {
	Filename string
	Data     []byte
}

func Create(artifacts []Artifact, outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, a := range artifacts {
		header := &tar.Header{
			Name:    a.Filename,
			Size:    int64(len(a.Data)),
			Mode:    0644,
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", a.Filename, err)
		}
		if _, err := tw.Write(a.Data); err != nil {
			return fmt.Errorf("writing tar data for %s: %w", a.Filename, err)
		}
	}

	return nil
}
