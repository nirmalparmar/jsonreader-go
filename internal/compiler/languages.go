package compiler

// Language describes how to build and run one language. Commands run with the
// work directory as cwd; "{box}" expands to that directory's in-jail path.
type Language struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Aliases  []string `json:"aliases,omitempty"`
	File     string   `json:"file"`
	Version  string   `json:"version"`
	Compiled bool     `json:"compiled"`

	compile []string
	run     []string
	probe   []string // must exit 0 for the language to be offered
	env     []string
	seed    string // optional dir copied to {box}/.cache/go before compiling (warm Go build cache)
	// Per-language limit overrides (0 = service default).
	memoryMB        int
	compileMemoryMB int
	procs           int
}

func builtinLanguages() []*Language {
	return []*Language{
		{
			ID: "python", Name: "Python 3", Aliases: []string{"py", "python3"}, File: "main.py",
			run:   []string{"python3", "-u", "main.py"},
			probe: []string{"python3", "--version"},
			env:   []string{"PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1", "PYTHONIOENCODING=utf-8"},
		},
		{
			ID: "javascript", Name: "JavaScript (Node.js)", Aliases: []string{"js", "node", "nodejs"}, File: "main.js",
			run:      []string{"node", "--max-old-space-size=192", "--stack-size=4096", "main.js"},
			probe:    []string{"node", "--version"},
			env:      []string{"NODE_OPTIONS=", "NO_COLOR=1"},
			memoryMB: 384,
		},
		{
			ID: "typescript", Name: "TypeScript (Node.js)", Aliases: []string{"ts"}, File: "main.ts",
			run:      []string{"node", "--experimental-strip-types", "--no-warnings", "--max-old-space-size=192", "main.ts"},
			probe:    []string{"node", "--experimental-strip-types", "--no-warnings", "-e", "const v: string = process.version; console.log(\"Node.js \" + v)"},
			env:      []string{"NODE_OPTIONS=", "NO_COLOR=1"},
			memoryMB: 384,
		},
		{
			ID: "c", Name: "C (GCC)", Aliases: []string{"gcc"}, File: "main.c", Compiled: true,
			compile: []string{"gcc", "-O2", "-std=gnu17", "-pipe", "-Wall", "-o", "main", "main.c", "-lm"},
			run:     []string{"./main"},
			probe:   []string{"gcc", "--version"},
		},
		{
			ID: "cpp", Name: "C++ (G++)", Aliases: []string{"c++", "g++", "cplusplus"}, File: "main.cpp", Compiled: true,
			compile:         []string{"g++", "-O2", "-std=gnu++20", "-pipe", "-Wall", "-o", "main", "main.cpp", "-lm"},
			run:             []string{"./main"},
			probe:           []string{"g++", "--version"},
			compileMemoryMB: 768,
		},
		{
			ID: "go", Name: "Go", Aliases: []string{"golang"}, File: "main.go", Compiled: true,
			compile: []string{"go", "build", "-trimpath", "-o", "main", "main.go"},
			run:     []string{"./main"},
			probe:   []string{"go", "version"},
			env:     []string{"GOCACHE={box}/.cache/go", "GOPATH={box}/.cache/gopath", "GOFLAGS=-mod=mod", "GOPROXY=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOTELEMETRY=off"},
			// Built by the Dockerfile with the same flags/env, so std packages are cache hits.
			seed:            "/opt/gocache",
			compileMemoryMB: 1024,
			procs:           128,
		},
		{
			ID: "java", Name: "Java (OpenJDK)", File: "Main.java",
			// Single-file source launcher: compiles in memory and runs.
			run:      []string{"java", "-XX:+UseSerialGC", "-XX:TieredStopAtLevel=1", "-Xss8m", "-Xmx256m", "-XX:MaxMetaspaceSize=128m", "-XX:ReservedCodeCacheSize=64m", "-Xshare:auto", "-Dfile.encoding=UTF-8", "Main.java"},
			probe:    []string{"java", "-version"},
			memoryMB: 768,
			procs:    128,
		},
		{
			ID: "rust", Name: "Rust", Aliases: []string{"rs"}, File: "main.rs", Compiled: true,
			compile:         []string{"rustc", "-O", "--edition", "2021", "-o", "main", "main.rs"},
			run:             []string{"./main"},
			probe:           []string{"rustc", "--version"},
			compileMemoryMB: 1024,
			procs:           128,
		},
		{
			ID: "ruby", Name: "Ruby", Aliases: []string{"rb"}, File: "main.rb",
			run:   []string{"ruby", "main.rb"},
			probe: []string{"ruby", "--version"},
		},
		{
			ID: "php", Name: "PHP", File: "main.php",
			run:   []string{"php", "-d", "display_errors=stderr", "main.php"},
			probe: []string{"php", "--version"},
		},
		{
			ID: "bash", Name: "Bash", Aliases: []string{"sh", "shell"}, File: "main.sh",
			run:   []string{"bash", "main.sh"},
			probe: []string{"bash", "--version"},
		},
	}
}
