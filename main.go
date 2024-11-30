package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	NoRoot = errors.New("")
)

func FindRoot(p string) (string, error) {
	root, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	for root != "" && root != "/" {
		files, err := os.ReadDir(root)
		if err != nil {
			return "", err
		}
		for _, file := range files {
			if !file.Type().IsRegular() {
				continue
			}

			if file.Name() == "go.mod" {
				return root, nil
			}
		}
		root = filepath.Dir(root)
	}

	return "", NoRoot
}

type ExecInfo struct {
	java     string
	gobraJar string
	root     string
}

func Gobra(ctx context.Context, ei ExecInfo, pkg string) *exec.Cmd {

	cmd := exec.CommandContext(ctx,
		ei.java,
		"-Xss1g",
		"-Xmx4g",
		"-jar", ei.gobraJar,
		"--backend", "SILICON",
		"--chop", "1",
		"--cacheFile", ".gobra/cache.json",
		"--onlyFilesWithHeader",
		"--assumeInjectivityOnInhale",
		"--checkConsistency",
		"--mceMode=on",
		"--moreJoins",
		"off",
		"-g", "/tmp/",
		"-I", ei.root,
		"-p", pkg,
	)

	return cmd
}

type Line struct {
	Lnum int
	Val  string
}

func lines(s string) []Line {
	lines := strings.Split(s, "\n")
	res := make([]Line, len(lines))

	for i, line := range lines {
		res[i] = Line{i, line}
	}

	return res
}

func comments(s string) []Line {

	res := make([]Line, 0)
	lines := strings.Split(s, "\n")

	for Lnum, Val := range lines {
		Val = strings.TrimSpace(Val)
		if !strings.HasPrefix(Val, "//") {
			continue
		}
		res = append(res, Line{Lnum: Lnum, Val: Val})
	}

	return res
}

func TrimGoComment(comment string) (string, bool) {
	comment = strings.TrimSpace(comment)
	comment, ok := strings.CutPrefix(comment, "//")
	if !ok {
		return "", false
	}
	comment = strings.TrimSpace(comment)
	comment, ok = strings.CutPrefix(comment, "@")
	return comment, ok
}

func IsAssert(comment string) bool {
	com, ok := TrimGoComment(comment)
	if ok {
		comment = com
	}

	comment = strings.TrimSpace(comment)

	return strings.HasPrefix(comment, "assert")
}

func IsGobraComment(comment string) bool {
	_, ok := TrimGoComment(comment)
	return ok
}

func HasHeader(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	return bytes.Contains(b, []byte("+gobra"))
}

func FilesWithHeader(dir string) ([]string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	res := make([]string, 0)
	for _, file := range files {
		if HasHeader(filepath.Join(dir, file.Name())) {
			res = append(res, file.Name())
		}
	}

	return res, nil
}

func ChopOne(s string, indent int) string {
	oldline := strings.ReplaceAll(s, "\t", "")
	return strings.Repeat("\t", indent) + "//chop! " + strings.ReplaceAll(oldline, "@", "#")
}

func ChopLine(s string, line int) string {
	lines := strings.Split(s, "\n")
	indent := strings.Count(lines[line], "\t")

	lines[line] = ChopOne(lines[line], indent)
	return strings.Join(lines, "\n")
}

type Ctx struct {
	/// files maps file names to their contents
	files map[string]string
	/// where we work on
	workDir string
	/// where we output results
	outDir string

	ei      ExecInfo
	tryChop func(string) bool
}

func NewCtx(pkg, outdir string, ei ExecInfo) (Ctx, error) {

	workDir, err := os.MkdirTemp(os.TempDir(), "meow")
	if err != nil {
		return Ctx{}, err
	}

	files := make(map[string]string)
	filesWithHeader, err := FilesWithHeader(pkg)
	if err != nil {
		return Ctx{}, nil
	}

	for _, file := range filesWithHeader {
		contents, err := os.ReadFile(filepath.Join(pkg, file))
		if err != nil {
			return Ctx{}, nil
		}

		files[file] = string(contents)
	}

	return Ctx{
		workDir: workDir,
		ei:      ei,
		files:   files,
		outDir:  outdir,
		tryChop: func(s string) bool {
			return IsGobraComment(s) && IsAssert(s)
		},
	}, nil
}

func (r *Ctx) WriteOut(fileName, contents string) error {
	return os.WriteFile(filepath.Join(r.outDir, fileName), []byte(contents), 0666)
}

func (r *Ctx) WriteWork(fileName, contents string) error {
	return os.WriteFile(filepath.Join(r.workDir, fileName), []byte(contents), 0666)
}

func (r *Ctx) RunWith(ctx context.Context, fileName string, contents string) (*exec.Cmd, error) {
	for f, contents := range r.files {
		if err := r.WriteWork(f, contents); err != nil {
			return nil, err
		}
	}

	if err := r.WriteWork(fileName, contents); err != nil {
		return nil, err
	}
	child := Gobra(ctx, r.ei, r.workDir)
	return child, nil
}

func (r *Ctx) tryToRemoveLine(duration time.Duration, fileName string, contents string, line int) (bool, error) {
	contents = ChopLine(contents, line)
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	child, err := r.RunWith(ctx, fileName, contents)
	if err != nil {
		return false, err
	}
	pipe, err := child.StdoutPipe()
	if err != nil {
		return false, err
	}

	ch := make(chan bool)

	go func() {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Println("> " + line)
			if strings.Contains(line, "ERROR") {
				ch <- false
				return
			}
			if strings.Contains(line, "Gobra has found 0 error(s)") {
				ch <- true
				return
			}
		}

		close(ch)
	}()

	child.Run()

	res, ok := <-ch
	if !ok {
		return false, nil
	}

	return res, nil
}

// trys to remove a single assert from the file and returns (line, true, nil) if we were able to remove it
func (r *Ctx) singlePassFile(duration time.Duration, fileName string, contents string, comments []Line) (int, bool, error) {

	for _, comment := range comments {
		if !r.tryChop(comment.Val) {
			continue
		}

		start := time.Now()
		fmt.Printf("try to remove line #%v in %v\n", comment.Lnum, fileName)
		ok, err := r.tryToRemoveLine(duration, fileName, contents, comment.Lnum)
		end := time.Now()
		took := end.Sub(start)
		if err != nil {
			return 0, false, err
		}

		if ok {
			fmt.Printf("ok after %v\n", took)
			return comment.Lnum, true, nil
		}
		fmt.Printf("failed after %v\n", took)
	}

	return 0, false, nil
}

func findLine(lines []Line, lnum int) int {

	cut := -1
	for i, comment := range lines {
		if comment.Lnum == lnum {
			cut = i
		}
	}
	return cut
}

func rotateAndDrop(lines []Line, lnum int) []Line {
	cut := findLine(lines, lnum)
	if cut == -1 {
		fmt.Printf("what? cut was -1 but there should be a comment withthis line num: %v\n", lnum)
		return lines
	}

	return append(lines[cut+1:], lines[:cut]...)
}

func (r *Ctx) maximallyReduce(duration time.Duration, fileName string) (string, error) {
	contents := r.files[fileName]
	// comments := comments(contents)
	comments := lines(contents)
	nRemoved := 0
	for {
		line, ok, err := r.singlePassFile(duration, fileName, contents, comments)
		if err != nil {
			return "", err
		}

		if !ok {
			break
		}

		nRemoved += 1

		contents = ChopLine(contents, line)

		r.WriteOut(fileName+".working", contents)
		lines := strings.Split(contents, "\n")
		fmt.Printf("GOOD: removing %v in %v would be fine: %v\n", line+1, fileName, lines[line])
		fmt.Printf("we now have removed %v lines\n", nRemoved)

		comments = rotateAndDrop(comments, line)
		// ln := make([]int, 0, len(comments))
		// for _, c := range comments {
		// 	ln = append(ln, c.Lnum)
		// }
		// fmt.Printf("ln: %v\n", ln)
	}

	return contents, nil
}

func main() {
	gobraEnv, _ := os.LookupEnv("GOBRA")
	gobraFlag := flag.String("gobra", gobraEnv, "path to gobra jar")
	baselineFlag := flag.String("baseline", "", "how many seconds we wait for a good answer. the default is time(no change) + 10%")
	javaFlag := flag.String("java", "java", "")
	outputDirFlag := flag.String("output", ".", "output directory")
	pattern := flag.String("pattern", "", "pattern of lines to try to chop")
	flag.Parse()

	pkg := flag.Arg(0)
	fmt.Println(*gobraFlag)
	fmt.Println(*javaFlag)
	fmt.Println(*outputDirFlag)
	fmt.Println(*baselineFlag)
	fmt.Println(pkg)

	if pkg == "" {
		fmt.Println("usage: minify-gobra [flags] <path/to/pkg>")
		flag.Usage()
		os.Exit(1)
	}

	if *gobraFlag == "" {
		fmt.Println("GOBRA env was not set and --gobra was not provided")
		os.Exit(1)
	}

	root, err := FindRoot(pkg)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	ei := ExecInfo{java: *javaFlag, gobraJar: *gobraFlag, root: root}
	r, err := NewCtx(pkg, *outputDirFlag, ei)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if *pattern != "" {
		regex, err := regexp.Compile(*pattern)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		r.tryChop = func(s string) bool {
			return regex.MatchString(s)
		}
	}

	if len(r.files) == 0 {
		fmt.Println("WARNING: no files found")
	}

	duration, err := time.ParseDuration(*baselineFlag)
	if err != nil {
		fmt.Println("running gobra on baseline")
		start := time.Now()
		cmd := Gobra(context.Background(), ei, pkg)
		cmd.Run()
		fmt.Println("done running gobra on baseline")
		duration = time.Since(start)
		duration += duration / 10
	}
	fmt.Printf("using duration %v\n", duration)

	for file := range r.files {
		fmt.Println("reducing", file)
		r.maximallyReduce(duration, file)
	}

}
