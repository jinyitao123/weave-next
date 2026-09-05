// goimports inventories source imports independently of build tags and GOOS.
package main

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type sourceFile struct {
	Path    string   `json:"path"`
	Imports []string `json:"imports"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: goimports REPOSITORY_ROOT")
		os.Exit(1)
	}
	files, err := inventory(os.Args[1])
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(files)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func inventory(root string) ([]sourceFile, error) {
	files := []sourceFile{}
	for _, directory := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				switch entry.Name() {
				case "vendor", "node_modules", ".git", "dist":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			file := sourceFile{Path: filepath.ToSlash(rel), Imports: []string{}}
			for _, imp := range parsed.Imports {
				value, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				file.Imports = append(file.Imports, value)
			}
			files = append(files, file)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}
