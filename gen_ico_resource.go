//go:build ignore

package main

import (
	"fmt"
	"image"
	_ "image/png" // register PNG decoder
	"os"

	"github.com/tc-hib/winres"
)

func main() {
	rs := winres.ResourceSet{}
	const fname = "winbollocks"
	// 1. Open your source PNG file
	pngFile, err := os.Open(fname + ".png")
	if err != nil {
		fmt.Printf("Error opening %s.png: %v\n", fname, err)
		os.Exit(1)
	}
	defer pngFile.Close()

	img, _, err := image.Decode(pngFile)
	if err != nil {
		fmt.Printf("Error decoding PNG: %v\n", err)
		os.Exit(1)
	}

	// 2. Convert PNG into a multi-resolution winres.Icon group
	icon, err := winres.NewIconFromResizedImage(img, nil)
	if err != nil {
		fmt.Printf("Error creating icon group: %v\n", err)
		os.Exit(1)
	}

	// 3. Save the generated multi-resolution icon to app.ico on disk
	icoFile, err := os.Create(fname + ".ico")
	if err != nil {
		fmt.Printf("Error creating %s.ico: %v\n", fname, err)
		os.Exit(1)
	}
	if err := icon.SaveICO(icoFile); err != nil {
		icoFile.Close()
		fmt.Printf("Error writing %s: %v\n", icoFile.Name(), err)
		os.Exit(1)
	}
	icoFile.Close()
	fmt.Printf("Successfully saved %s to disk.\n", icoFile.Name())

	// 4. Set the icon as Resource ID 1 (Standard executable icon ID)
	rs.SetIcon(winres.ID(1), icon)

	// 5. Write out the COFF object file for x64 Windows
	sysoFile, err := os.Create("rsrc_windows_amd64.syso")
	if err != nil {
		fmt.Printf("Error creating .syso file %s, err: %v\n", sysoFile.Name(), err)
		os.Exit(1)
	}
	defer sysoFile.Close()

	if err := rs.WriteObject(sysoFile, winres.ArchAMD64); err != nil {
		fmt.Printf("Error writing object %s: %v\n", sysoFile.Name(), err)
		os.Exit(1)
	}

	fmt.Printf("Successfully generated %s\n", sysoFile.Name())
}
