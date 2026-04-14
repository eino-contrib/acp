package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultSchemaPath      = "./schema/schema.json"
	defaultMetaPath        = "./schema/meta.json"
	defaultOutputPath      = "../../types_gen.go"
	defaultDownloadTimeout = 30 * time.Second

	defaultSchemaURL = "https://raw.githubusercontent.com/agentclientprotocol/agent-client-protocol/main/schema/schema.json"
	defaultMetaURL   = "https://raw.githubusercontent.com/agentclientprotocol/agent-client-protocol/main/schema/meta.json"
)

type cliLayout int

const (
	cliLayoutUnknown cliLayout = iota
	cliLayoutRepoRoot
	cliLayoutGenerateDir
)

func downloadFile(url, dest string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultDownloadTimeout
	}
	fmt.Fprintf(os.Stderr, "Downloading %s -> %s\n", url, dest)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request for %s: %w", url, err)
	}

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: HTTP %d", url, resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func resolveCLIPaths(cwd, schemaPath, metaPath, output string) (string, string, string) {
	layout := detectCLILayout(cwd)

	if schemaPath == defaultSchemaPath {
		schemaPath = resolveDefaultInputPath(cwd, layout, "schema.json")
	}
	if metaPath == defaultMetaPath {
		metaPath = resolveDefaultInputPath(cwd, layout, "meta.json")
	}
	if output == defaultOutputPath {
		output = resolveDefaultOutputPath(cwd, layout)
	}

	return filepath.Clean(schemaPath), filepath.Clean(metaPath), filepath.Clean(output)
}

func detectCLILayout(cwd string) cliLayout {
	if fileExists(filepath.Join(cwd, "go.mod")) && fileExists(filepath.Join(cwd, "cmd", "generate", "main.go")) {
		return cliLayoutRepoRoot
	}
	if filepath.Base(cwd) == "generate" &&
		filepath.Base(filepath.Dir(cwd)) == "cmd" &&
		fileExists(filepath.Join(cwd, "main.go")) &&
		fileExists(filepath.Join(cwd, "..", "..", "go.mod")) {
		return cliLayoutGenerateDir
	}
	return cliLayoutUnknown
}

func resolveDefaultInputPath(cwd string, layout cliLayout, fileName string) string {
	switch layout {
	case cliLayoutRepoRoot:
		return filepath.Join(cwd, "cmd", "generate", "schema", fileName)
	case cliLayoutGenerateDir:
		return filepath.Join(cwd, "schema", fileName)
	default:
		return filepath.Join(cwd, "schema", fileName)
	}
}

func resolveDefaultOutputPath(cwd string, layout cliLayout) string {
	switch layout {
	case cliLayoutRepoRoot:
		return filepath.Join(cwd, "types_gen.go")
	case cliLayoutGenerateDir:
		return filepath.Join(cwd, "..", "..", "types_gen.go")
	default:
		return filepath.Join(cwd, defaultOutputPath)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func main() {
	schemaPath := flag.String("schema", defaultSchemaPath, "path to schema.json")
	metaPath := flag.String("meta", defaultMetaPath, "path to meta.json")
	output := flag.String("output", defaultOutputPath, "output file path")
	pkg := flag.String("package", "acp", "Go package name")
	download := flag.Bool("download", false, "download latest schema and meta from GitHub before generating")
	schemaURL := flag.String("schema-url", defaultSchemaURL, "schema.json URL to download when -download is set")
	metaURL := flag.String("meta-url", defaultMetaURL, "meta.json URL to download when -download is set")
	downloadTimeout := flag.Duration("download-timeout", defaultDownloadTimeout, "timeout for each schema/meta download when -download is set")
	flag.Parse()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving working directory: %v\n", err)
		os.Exit(1)
	}

	resolvedSchemaPath, resolvedMetaPath, resolvedOutput := resolveCLIPaths(cwd, *schemaPath, *metaPath, *output)

	if *download {
		if err := downloadFile(*schemaURL, resolvedSchemaPath, *downloadTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "Error downloading schema: %v\n", err)
			os.Exit(1)
		}
		if err := downloadFile(*metaURL, resolvedMetaPath, *downloadTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "Error downloading meta: %v\n", err)
			os.Exit(1)
		}
	}

	schema, err := LoadSchema(resolvedSchemaPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading schema: %v\n", err)
		os.Exit(1)
	}

	meta, err := LoadMeta(resolvedMetaPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading meta: %v\n", err)
		os.Exit(1)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate(*pkg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating code: %v\n", err)
		os.Exit(1)
	}

	// Ensure output directory exists
	dir := filepath.Dir(resolvedOutput)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating directory: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(resolvedOutput, src, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Generated %s (%d bytes)\n", resolvedOutput, len(src))

	// Generate client and agent interface files
	clientSrc, agentSrc, err := gen.GenerateInterfaces(*pkg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating interfaces: %v\n", err)
		os.Exit(1)
	}
	methodsSrc, err := gen.GenerateMethodMetadata("methodmeta")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating method metadata: %v\n", err)
		os.Exit(1)
	}

	outputDir := filepath.Dir(resolvedOutput)
	clientFile := filepath.Join(outputDir, "client_gen.go")
	agentFile := filepath.Join(outputDir, "agent_gen.go")
	methodmetaDir := filepath.Join(outputDir, "internal", "methodmeta")
	if err := os.MkdirAll(methodmetaDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating methodmeta directory: %v\n", err)
		os.Exit(1)
	}
	methodsFile := filepath.Join(methodmetaDir, "metadata_gen.go")

	if err := os.WriteFile(clientFile, clientSrc, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", clientFile, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Generated %s (%d bytes)\n", clientFile, len(clientSrc))

	if err := os.WriteFile(agentFile, agentSrc, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", agentFile, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Generated %s (%d bytes)\n", agentFile, len(agentSrc))

	if err := os.WriteFile(methodsFile, methodsSrc, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", methodsFile, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Generated %s (%d bytes)\n", methodsFile, len(methodsSrc))

	// Generate conn package files (outbound methods + handler registration)
	agentOutSrc, clientOutSrc, handlersSrc, err := gen.GenerateConnFiles("conn")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating conn files: %v\n", err)
		os.Exit(1)
	}

	connDir := filepath.Join(outputDir, "conn")
	if err := os.MkdirAll(connDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating conn directory: %v\n", err)
		os.Exit(1)
	}

	agentOutFile := filepath.Join(connDir, "agent_outbound_gen.go")
	clientOutFile := filepath.Join(connDir, "client_outbound_gen.go")
	handlersFile := filepath.Join(connDir, "handlers_gen.go")

	for _, f := range []struct {
		path string
		src  []byte
	}{
		{agentOutFile, agentOutSrc},
		{clientOutFile, clientOutSrc},
		{handlersFile, handlersSrc},
	} {
		if err := os.WriteFile(f.path, f.src, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", f.path, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Generated %s (%d bytes)\n", f.path, len(f.src))
	}

	// Verify generated output against schema
	result, err := Verify(schema, resolvedOutput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Verify error: %v\n", err)
		os.Exit(1)
	}
	clientVerify, err := VerifyInterfaceFile(clientFile, "Client", gen.buildMethods(meta.ClientMethods, "client"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Client interface verify error: %v\n", err)
		os.Exit(1)
	}
	agentVerify, err := VerifyInterfaceFile(agentFile, "Agent", gen.buildMethods(meta.AgentMethods, "agent"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Agent interface verify error: %v\n", err)
		os.Exit(1)
	}
	result.ClientInterface = clientVerify
	result.AgentInterface = agentVerify
	PrintVerifyReport(result)
	if len(result.Missing) > 0 || len(result.AnyFallbacks) > 0 ||
		len(clientVerify.MissingMethods) > 0 || len(clientVerify.MissingConstants) > 0 || len(clientVerify.BadSignatures) > 0 ||
		len(agentVerify.MissingMethods) > 0 || len(agentVerify.MissingConstants) > 0 || len(agentVerify.BadSignatures) > 0 {
		os.Exit(1)
	}
}
