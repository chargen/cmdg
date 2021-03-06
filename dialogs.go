package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"sort"
	"time"

	gc "github.com/rthornton128/goncurses"

	"github.com/ThomasHabets/cmdg/ncwrap"
)

var (
	errCancel = fmt.Errorf("user cancelled request")
	errOpen   = fmt.Errorf("user asked to open, not save")
)

type sortFiles []os.FileInfo

func (a sortFiles) Len() int      { return len(a) }
func (a sortFiles) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a sortFiles) Less(i, j int) bool {
	if a[i].IsDir() && !a[j].IsDir() {
		return true
	}
	if !a[i].IsDir() && a[j].IsDir() {
		return false
	}
	return a[i].Name() < a[j].Name()
}

type dotDirs struct {
	name string
	fi   os.FileInfo
}

func (f *dotDirs) Name() string       { return f.name }
func (f *dotDirs) Size() int64        { return f.fi.Size() }
func (f *dotDirs) Mode() os.FileMode  { return f.fi.Mode() }
func (f *dotDirs) ModTime() time.Time { return f.fi.ModTime() }
func (f *dotDirs) IsDir() bool        { return f.fi.IsDir() }
func (f *dotDirs) Sys() interface{}   { return f.fi.Sys() }

// like os.ReadDir, except includes dot and double-dot, and sorted.
func readDir(d string) ([]os.FileInfo, error) {
	dot, err := os.Stat(path.Join(d, "."))
	if err != nil {
		return nil, err
	}

	dots, err := os.Stat(path.Join(d, ".."))
	if err != nil {
		return nil, err
	}

	files, err := ioutil.ReadDir(d)
	if err != nil {
		return nil, err
	}
	sort.Sort(sortFiles(files))
	files = append([]os.FileInfo{
		&dotDirs{name: ".", fi: dot},
		&dotDirs{name: "..", fi: dots},
	}, files...)
	return files, nil
}

func fullscreenWindow() *gc.Window {
	maxY, maxX := winSize()

	w, err := gc.NewWindow(maxY-5, maxX-4, 2, 2)
	if err != nil {
		log.Fatalf("Creating stringChoice window: %v", err)
	}
	return w
}

func loadFileDialog() (string, error) {
	w := fullscreenWindow()
	defer w.Delete()

	cur := 0
	curDir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	files, err := readDir(curDir)
	if err != nil {
		return "", err
	}

	for {
		w.Clear()
		w.Print("\n")
		offset := 0
		if cur > 5 {
			offset = cur - 5
		}
		for n, f := range files {
			if n < offset {
				continue
			}
			printName := f.Name()
			if f.IsDir() {
				printName += "/"
			}
			if n == cur {
				w.Print(fmt.Sprintf(" > %s\n", printName))
			} else {
				w.Print(fmt.Sprintf("   %s\n", printName))
			}
		}
		winBorder(w)
		w.Refresh()
		select {
		case key := <-nc.Input:
			switch key {
			case 'q':
				return "", errCancel
			case 'n':
				if cur < len(files)-1 {
					cur++
				}
			case 'p':
				if cur > 0 {
					cur--
				}
			case '\n':
				if files[cur].IsDir() {
					newDir := path.Join(curDir, files[cur].Name())
					newFiles, err := readDir(newDir)
					if err == nil {
						curDir = newDir
						files = newFiles
						cur = 0
					}
				} else {
					return path.Join(curDir, files[cur].Name()), nil
				}
			}
		}
	}
}

// saveFileDialog finds a place to save a file.
// Run in main/ncurses goroutine.
func saveFileDialog(fn string) (string, error) {
	defer func() {
		gc.Cursor(0)
	}()
	w := fullscreenWindow()
	defer w.Delete()

	cur := -1
	curDir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	files, err := readDir(curDir)
	if err != nil {
		return "", err
	}

	fileNameEdit := false
	for {
		w.Clear()
		filenamePrompt := "Filename> "
		w.Print(fmt.Sprintf("\n  %s%s\n  Current dir: %s\n\n", filenamePrompt, fn, curDir))
		prefix := "  "
		if cur == -1 {
			prefix = "[bold] >"
		}
		ncwrap.ColorPrint(w, fmt.Sprintf("%s <save>[unbold]\n", prefix))
		offset := 0
		if cur > 5 {
			offset = cur - 5
		}
		for n, f := range files {
			if n < offset {
				continue
			}
			printName := f.Name()
			if f.IsDir() {
				printName += "/"
			}
			if n == cur {
				ncwrap.ColorPrint(w, "[bold] > %s[unbold]\n", printName)
			} else {
				w.Print(fmt.Sprintf("   %s\n", printName))
			}
		}
		gc.Cursor(0)
		winBorder(w)
		w.Refresh()
		if fileNameEdit {
			w.Move(1, 2+len(filenamePrompt)+len(fn))
			gc.Cursor(1)
			w.Refresh()
			select {
			case key := <-nc.Input:
				switch key {
				case gc.KEY_TAB:
					fileNameEdit = false
				case '\n', '\r':
					return path.Join(curDir, fn), nil
				case '\b', gc.KEY_BACKSPACE, 127:
					if len(fn) > 0 {
						fn = fn[:len(fn)-1]
					}
				default:
					fn = fmt.Sprintf("%s%c", fn, key)
				}
			}
		} else {
			select {
			case key := <-nc.Input:
				switch key {
				case '?':
					helpWin(`q, ^C, ^G         Abort save
!                 Open as temp file.
^P, n, k, Up      Previous
^N, p, j, Down    Next
Enter             Choose
Tab               Switch to file name editing
`)
				case gc.KEY_DOWN, 'n', ctrlN, 'k':
					if cur < len(files)-1 {
						cur++
					}
				case gc.KEY_UP, 'p', ctrlP, 'j':
					// -1 is OK, it's the OK button.
					if cur >= 0 {
						cur--
					}
				case '!':
					return "", errOpen
				case 'q', ctrlC, ctrlG:
					return "", errCancel
				case gc.KEY_TAB:
					fileNameEdit = true
				case '\n', '\r':
					if cur == -1 {
						return path.Join(curDir, fn), nil
					}
					if files[cur].IsDir() {
						newDir := path.Join(curDir, files[cur].Name())
						newFiles, err := readDir(newDir)
						if err == nil {
							curDir = newDir
							files = newFiles
							cur = 0
						}
					}
				}
			}
		}
	}
}
