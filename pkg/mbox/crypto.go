package mbox

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/revv00/mailfs/pkg/crypto"
)

const (
	ConfigFileName = "config.json"
	DBFileName     = "mailfs.db"
	BlobDBFileName = "mailfs-blob.db"
)

// Pack creates an encrypted stick file from config data and database files
func Pack(password string, configData []byte, dbPath string, outputFile string) error {
	outFile, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	tarBuf := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuf)

	// Add Config
	if err := addFileToTar(tw, ConfigFileName, configData); err != nil {
		return fmt.Errorf("failed to add config to tar: %w", err)
	}

	// Add JuiceFS DB
	if dbPath != "" {
		dbData, err := os.ReadFile(dbPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to read db file: %w", err)
		}
		if err == nil {
			if err := addFileToTar(tw, DBFileName, dbData); err != nil {
				return fmt.Errorf("failed to add db to tar: %w", err)
			}
		}

		// Add MailFS Blob DB (Mapping)
		blobDBPath := filepath.Join(filepath.Dir(dbPath), BlobDBFileName)
		blobDBData, err := os.ReadFile(blobDBPath)
		if err != nil && !os.IsNotExist(err) {
			// Don't fail if blob DB is missing, but it really should be there
		}
		if err == nil {
			if err := addFileToTar(tw, BlobDBFileName, blobDBData); err != nil {
				return fmt.Errorf("failed to add blob db to tar: %w", err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}

	ciphertext, err := crypto.Encrypt(password, tarBuf.Bytes())
	if err != nil {
		return err
	}

	if _, err := outFile.Write(ciphertext); err != nil {
		return err
	}

	return nil
}

// Unpack decrypts a stick file to a destination directory
func Unpack(password string, stickFile string, destDir string) error {
	ciphertext, err := os.ReadFile(stickFile)
	if err != nil {
		return err
	}

	plaintext, err := crypto.Decrypt(password, ciphertext)
	if err != nil {
		return err
	}

	// Untar
	tr := tar.NewReader(bytes.NewReader(plaintext))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)
		outFile, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(outFile, tr); err != nil {
			outFile.Close()
			return err
		}
		outFile.Close()
	}

	return nil
}

func addFileToTar(tw *tar.Writer, name string, data []byte) error {
	header := &tar.Header{
		Name: name,
		Mode: 0600,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
