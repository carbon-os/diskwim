package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Microsoft/go-winio/wim"
)

func main() {
	// 1. Define CLI flags
	inputPath := flag.String("input", "", "Path to the .wim file")
	lsPath := flag.String("ls", "/", "Directory path inside the WIM to list")
	flag.Parse()

	if *inputPath == "" {
		fmt.Println("Usage: wim -input <file.wim> -ls <path>")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// 2. Open the WIM file
	f, err := os.Open(*inputPath)
	if err != nil {
		log.Fatalf("Failed to open WIM: %v", err)
	}
	defer f.Close()

	w, err := wim.Open(f)
	if err != nil {
		log.Fatalf("Failed to read WIM header: %v", err)
	}
	defer w.Close()

	// 3. Select the first image (Index 1)
	// WIMs can contain multiple images; for this CLI we'll default to the first.
	img, err := w.Image(1)
	if err != nil {
		log.Fatalf("Failed to open image index 1: %v", err)
	}

	// 4. Navigate to the requested directory
	root, err := img.Root()
	if err != nil {
		log.Fatalf("Failed to get root entry: %v", err)
	}

	targetDir := findEntry(root, *lsPath)
	if targetDir == nil {
		log.Fatalf("Path '%s' not found in WIM", *lsPath)
	}

	// 5. List the contents
	fmt.Printf("Listing contents of: %s\n", *lsPath)
	fmt.Println("Mode        Size          Name")
	fmt.Println("----        ----          ----")

	children, err := targetDir.Children()
	if err != nil {
		log.Fatalf("Failed to list children: %v", err)
	}

	for _, child := range children {
		info := child.Stat()
		// Simple formatting for the list output
		fmt.Printf("%-11s %-13d %s\n", 
			info.Mode().String(), 
			info.Size(), 
			info.Name())
	}
}

// findEntry traverses the WIM tree based on a slash-separated path
func findEntry(root *wim.Entry, path string) *wim.Entry {
	cleanPath := strings.Trim(path, "/")
	if cleanPath == "" {
		return root
	}

	parts := strings.Split(cleanPath, "/")
	current := root

	for _, part := range parts {
		children, err := current.Children()
		if err != nil {
			return nil
		}

		found := false
		for _, child := range children {
			if strings.EqualFold(child.Stat().Name(), part) {
				current = child
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return current
}