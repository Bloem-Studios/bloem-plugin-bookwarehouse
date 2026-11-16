package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	pluginv1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// This exercises the installed process boundary, not an in-process server:
// removing either ServeManifest option, skipping verification before streaming,
// or losing the requested revision/range fails an observable RPC assertion.
func TestExecutablePlugin(t *testing.T) {
	binary := buildExecutable(t)
	manifest := executableManifest(t, binary)

	t.Run("manifest and verified range reads", func(t *testing.T) {
		backend := newBookwarehouseFixture(t)
		client := launchExecutable(t, binary)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		gotManifest, err := client.Runtime().GetManifest(ctx, &pluginv1.GetManifestRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(gotManifest.GetManifest(), manifest) {
			t.Fatal("RPC manifest differs from executable manifest CLI")
		}
		configureExecutable(t, ctx, client, backend)
		storage := storagev1.NewStorageProviderClient(client.Conn())
		folderID := "epub"
		for _, name := range []string{"private", "Archive"} {
			page, err := storage.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: folderID, DirectoriesOnly: true})
			if err != nil || len(page.GetEntries()) != 1 || page.Entries[0].Name != name || !page.Complete {
				t.Fatalf("folder browse: %v %v", page, err)
			}
			folderID = page.Entries[0].Id
			if _, err := storage.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: folderID}); err != nil {
				t.Fatal(err)
			}
		}
		if files := listAll(t, ctx, storage, folderID, 100); len(files) != 2 || files["epub/book-a"] == nil {
			t.Fatalf("scoped file inventory: %v", files)
		}
		description, err := storage.Describe(ctx, &storagev1.DescribeRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if description.GetRevision() != 1 || len(description.GetSources()) != 1 {
			t.Fatalf("Describe: %v", description)
		}
		source := description.GetSources()[0]
		if source.GetId() != "bookwarehouse" || source.GetName() != "Bookwarehouse" ||
			source.GetRootEntryId() != "root" || !source.GetRevisionPinnedReads() {
			t.Fatalf("source contract: %v", source)
		}
		root := listAll(t, ctx, storage, "root", 1)
		if len(root) != 2 {
			t.Fatalf("root entries: %v", root)
		}
		for _, id := range []string{"epub", "pdf"} {
			entry := root[id]
			if entry == nil || entry.GetKind() != storagev1.EntryKind_ENTRY_KIND_DIRECTORY ||
				entry.GetRevision() != "directories-v1" {
				t.Fatalf("directory %s: %v", id, entry)
			}
			if _, err := storage.Stat(ctx, &storagev1.StatRequest{
				SourceId: "bookwarehouse", EntryId: id, ExpectedRevision: "directories-v1",
			}); err != nil {
				t.Fatalf("Stat directory %s: %v", id, err)
			}
		}
		epubs := listAll(t, ctx, storage, "epub", 1)
		if len(epubs) != 2 || epubs["epub/book-a"] == nil || epubs["epub/book-b"] == nil {
			t.Fatalf("EPUB entries: %v", epubs)
		}
		pdfs := listAll(t, ctx, storage, "pdf", 1)
		if len(pdfs) != 1 || pdfs["pdf/book-pdf"] == nil {
			t.Fatalf("PDF entries: %v", pdfs)
		}
		entry := epubs["epub/book-a"]
		want := backend.books[0].body
		if entry.GetLogicalPath() != "epub/book-a.epub" ||
			entry.GetKind() != storagev1.EntryKind_ENTRY_KIND_FILE ||
			entry.GetSize() != int64(len(want)) || entry.GetRevision() != revision(want) {
			t.Fatalf("file contract: %v", entry)
		}
		stat, err := storage.Stat(ctx, &storagev1.StatRequest{
			SourceId: "bookwarehouse", EntryId: entry.GetId(), ExpectedRevision: entry.GetRevision(),
		})
		if err != nil || !proto.Equal(entry, stat.GetEntry()) {
			t.Fatalf("Stat: %v, %v", stat, err)
		}
		readExact(t, ctx, storage, entry, 0, want)
		backend.mu.Lock()
		backend.mode = "corrupt"
		backend.mu.Unlock()
		// Reauthorize the download but retain already verified immutable bytes.
		readExact(t, ctx, storage, entry, 123, want[123:140123])
		backend.mu.Lock()
		backend.mode = ""
		backend.mu.Unlock()
		readExact(t, ctx, storage, pdfs["pdf/book-pdf"], 0, backend.books[2].body)
		backend.mu.Lock()
		downloads, probes := backend.downloads["book-a"], backend.probes["book-a"]
		backend.mu.Unlock()
		if downloads != 1 || probes != 1 {
			t.Fatalf("each cached Read must reauthorize download access with a one-byte probe: got %d downloads and %d probes, want 1 and 1", downloads, probes)
		}
	})

	for _, scenario := range []struct {
		name string
		mode string
		code codes.Code
		warm bool
	}{
		{"metadata access denied", "metadata-denied", codes.PermissionDenied, false},
		{"download access denied", "download-denied", codes.PermissionDenied, false},
		{"warm cache download access denied", "download-denied", codes.PermissionDenied, true},
		{"corrupt full download", "corrupt", codes.FailedPrecondition, false},
		{"changed pinned revision", "changed", codes.FailedPrecondition, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			backend := newBookwarehouseFixture(t)
			client := launchExecutable(t, binary)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			configureExecutable(t, ctx, client, backend)
			storage := storagev1.NewStorageProviderClient(client.Conn())
			stat, err := storage.Stat(ctx, &storagev1.StatRequest{
				SourceId: "bookwarehouse", EntryId: "epub/book-a", ExpectedRevision: revision(backend.books[0].body),
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario.warm {
				readExact(t, ctx, storage, stat.GetEntry(), 0, backend.books[0].body[:32])
			}
			backend.mu.Lock()
			backend.mode = scenario.mode
			backend.mu.Unlock()
			if scenario.mode == "metadata-denied" {
				_, err := storage.Stat(ctx, &storagev1.StatRequest{
					SourceId: "bookwarehouse", EntryId: "epub/book-a", ExpectedRevision: stat.GetEntry().GetRevision(),
				})
				if status.Code(err) != codes.PermissionDenied {
					t.Fatalf("denied Stat = %v", err)
				}
			}
			stream, err := storage.Read(ctx, &storagev1.ReadRequest{
				SourceId: "bookwarehouse", EntryId: "epub/book-a",
				ExpectedRevision: stat.GetEntry().GetRevision(), Offset: 3, Length: 32,
			})
			if err == nil {
				var chunk *storagev1.ReadChunk
				chunk, err = stream.Recv()
				if chunk != nil {
					t.Fatalf("failed pinned read exposed a chunk: offset=%d bytes=%d", chunk.GetOffset(), len(chunk.GetData()))
				}
			}
			if status.Code(err) != scenario.code {
				t.Fatalf("Read = %v, want %s", err, scenario.code)
			}
			if strings.Contains(err.Error(), fixtureAPIKey) {
				t.Fatal("RPC error exposed the configured secret")
			}
		})
	}
}

// The request router serves from the same process and the same installation
// connection as storage: no host-supplied address or key is needed, and the
// installation refuses the content kind it does not serve.
func TestExecutableRequestRouter(t *testing.T) {
	binary := buildExecutable(t)
	backend := newBookwarehouseFixture(t)
	client := launchExecutable(t, binary)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	router := client.RequestRouter()
	connection := &pluginv1.RouterConnection{Id: "requests"}
	if _, err := router.Fulfill(ctx, &pluginv1.FulfillRequest{CapabilityId: "ebooks", Connections: []*pluginv1.RouterConnection{connection}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unconfigured Fulfill = %v", err)
	}
	configureExecutable(t, ctx, client, backend)
	got, err := router.TestConnection(ctx, &pluginv1.TestConnectionRequest{CapabilityId: "ebooks", Connection: connection})
	if err != nil || !got.GetOk() {
		t.Fatalf("ebook TestConnection = %v, %v", got, err)
	}
	if _, err := router.Validate(ctx, &pluginv1.ValidateRequest{CapabilityId: "audiobooks", Connection: connection}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an ebook installation must refuse audiobook requests: %v", err)
	}
	backend.mu.Lock()
	monitoring := backend.monitoring
	backend.mu.Unlock()
	if monitoring != 1 {
		t.Fatalf("monitoring reads = %d, want 1", monitoring)
	}
}

func buildExecutable(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("BOOKWAREHOUSE_TEST_BINARY"); binary != "" {
		absolute, err := filepath.Abs(binary)
		if err != nil {
			t.Fatal(err)
		}
		return absolute
	}
	binary := filepath.Join(t.TempDir(), "bloem-plugin-bookwarehouse")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	args := []string{"build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", binary, "."}
	cgo := "0"
	if os.Getenv("BOOKWAREHOUSE_TEST_RACE") == "1" {
		args = append(args[:1], append([]string{"-race"}, args[1:]...)...)
		cgo = "1"
	}
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgo, "GOOS="+runtime.GOOS,
		"GOARCH="+runtime.GOARCH, "GOTOOLCHAIN=local", "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build real executable: %v\n%s", err, output)
	}
	return binary
}

func executableManifest(t *testing.T, binary string) *pluginv1.PluginManifest {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "manifest").Output()
	if err != nil {
		t.Fatalf("manifest CLI: %v", err)
	}
	manifest := new(pluginv1.PluginManifest)
	if err := protojson.Unmarshal(output, manifest); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	if manifest.GetChecksum() != hex.EncodeToString(hash.Sum(nil)) {
		t.Fatal("manifest checksum is not the executable SHA-256")
	}
	if manifest.GetPluginId() != "bloem.storage.bookwarehouse" || manifest.GetVersion() != "0.4.0" ||
		manifest.GetSiloApiVersion() != "v1" || len(manifest.GetHttpRoutes()) != 0 {
		t.Fatalf("native manifest contract: %v", manifest)
	}
	// One request_router.v1 capability per content kind, with fixed IDs the
	// host binds request integrations to.
	wantKinds := map[string]string{"ebooks": "book", "audiobooks": "audiobook"}
	if len(manifest.GetCapabilities()) != len(wantKinds) {
		t.Fatalf("capabilities: %v", manifest.GetCapabilities())
	}
	for _, capability := range manifest.GetCapabilities() {
		kinds := capability.GetMetadata().GetFields()["supported_media_types"].GetListValue().GetValues()
		if capability.GetType() != "request_router.v1" || wantKinds[capability.GetId()] == "" || len(kinds) != 1 ||
			kinds[0].GetStringValue() != wantKinds[capability.GetId()] {
			t.Fatalf("capability contract: %v", capability)
		}
	}
	secrets := map[string]bool{}
	for _, schema := range manifest.GetGlobalConfigSchema() {
		if schema.GetKey() != "connection" {
			continue
		}
		for _, field := range schema.GetAdminForm().GetFields() {
			secrets[field.GetKey()] = field.GetSecret() && field.GetControl() == pluginv1.AdminFormControl_ADMIN_FORM_CONTROL_PASSWORD
		}
	}
	if !secrets["api_key"] || !secrets["request_api_key"] {
		t.Fatal("connection.api_key and connection.request_api_key must be secret password fields")
	}
	return manifest
}

func launchExecutable(t *testing.T, binary string) *sdkruntime.Client {
	t.Helper()
	cmd := exec.Command(binary)
	private := t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + private, "TMPDIR=" + private}
	var stderr lockedBuffer
	process := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  sdkruntime.HandshakeConfig(),
		Plugins:          sdkruntime.DefaultPluginSet(sdkruntime.CapabilityServers{}),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		StartTimeout:     10 * time.Second,
		Logger:           hclog.NewNullLogger(),
		Stderr:           &stderr,
	})
	t.Cleanup(func() {
		process.Kill()
		if strings.Contains(stderr.String(), "WARNING: DATA RACE") {
			t.Errorf("plugin subprocess race detector reported a race:\n%s", stderr.String())
		}
	})
	connection, err := process.Client()
	if err != nil {
		t.Fatalf("plugin handshake: %v", err)
	}
	dispensed, err := connection.Dispense(sdkruntime.PluginSetName)
	if err != nil {
		t.Fatalf("SDK dispense: %v", err)
	}
	client, ok := dispensed.(*sdkruntime.Client)
	if !ok {
		t.Fatalf("SDK client type = %T", dispensed)
	}
	return client
}

// go-plugin may write diagnostics concurrently with cleanup and assertions.
type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func configureExecutable(t *testing.T, ctx context.Context, client *sdkruntime.Client, backend *bookwarehouseFixture) {
	t.Helper()
	value, err := structpb.NewStruct(map[string]any{
		"base_url":            backend.server.URL + "/warehouse",
		"api_key":             fixtureAPIKey,
		"allow_insecure_http": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Runtime().Configure(ctx, &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: value}},
	}); err != nil {
		t.Fatalf("Runtime.Configure: %v", err)
	}
}

func listAll(t *testing.T, ctx context.Context, client storagev1.StorageProviderClient, directory string, limit uint32) map[string]*storagev1.Entry {
	t.Helper()
	entries := make(map[string]*storagev1.Entry)
	cursors := make(map[string]bool)
	cursor := ""
	for range 10 {
		page, err := client.List(ctx, &storagev1.ListRequest{
			SourceId: "bookwarehouse", DirectoryId: directory, Cursor: cursor, MaxEntries: limit,
		})
		if err != nil {
			t.Fatalf("List %s: %v", directory, err)
		}
		if len(page.GetEntries()) > int(limit) {
			t.Fatalf("List exceeded max_entries: %d", len(page.GetEntries()))
		}
		for _, entry := range page.GetEntries() {
			if entries[entry.GetId()] != nil {
				t.Fatalf("duplicate listing identity: %q", entry.GetId())
			}
			entries[entry.GetId()] = entry
		}
		if page.GetComplete() {
			if page.GetNextCursor() != "" {
				t.Fatal("completed page has a cursor")
			}
			return entries
		}
		cursor = page.GetNextCursor()
		if cursor == "" || cursors[cursor] {
			t.Fatal("pagination cursor did not advance")
		}
		cursors[cursor] = true
	}
	t.Fatal("listing did not terminate")
	return nil
}

func readExact(t *testing.T, ctx context.Context, client storagev1.StorageProviderClient, entry *storagev1.Entry, offset int64, want []byte) {
	t.Helper()
	stream, err := client.Read(ctx, &storagev1.ReadRequest{
		SourceId: "bookwarehouse", EntryId: entry.GetId(), ExpectedRevision: entry.GetRevision(),
		Offset: offset, Length: int64(len(want)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	final := false
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break // A successful final gRPC status is part of the assertion.
		}
		if err != nil {
			t.Fatalf("Read terminal status: %v", err)
		}
		if final || chunk.GetOffset() != offset+int64(len(got)) ||
			len(chunk.GetData()) > 128*1024 || len(chunk.GetData()) == 0 {
			t.Fatalf("invalid read chunk: offset=%d bytes=%d eof=%t", chunk.GetOffset(), len(chunk.GetData()), chunk.GetEof())
		}
		got = append(got, chunk.GetData()...)
		if len(got) > len(want) {
			t.Fatal("Read exceeded requested length")
		}
		final = chunk.GetEof()
	}
	if !final || !bytes.Equal(got, want) {
		t.Fatalf("Read: bytes=%d want=%d eof=%t", len(got), len(want), final)
	}
}

const fixtureAPIKey = "synthetic-test-key-never-a-real-credential"

type fixtureBook struct {
	id, format string
	body       []byte
}

type bookwarehouseFixture struct {
	server    *httptest.Server
	mu        sync.Mutex
	mode      string
	books     []fixtureBook
	downloads map[string]int
	probes    map[string]int
	// monitoring counts ebook request monitoring reads.
	monitoring int
}

func newBookwarehouseFixture(t *testing.T) *bookwarehouseFixture {
	t.Helper()
	backend := &bookwarehouseFixture{
		books: []fixtureBook{
			{"book-a", "epub", bytes.Repeat([]byte("verified EPUB bytes\n"), 17000)},
			{"book-b", "epub", []byte("second EPUB fixture")},
			{"book-pdf", "pdf", []byte("%PDF-1.7\nsynthetic PDF fixture\n%%EOF")},
		},
		downloads: make(map[string]int),
		probes:    make(map[string]int),
	}
	backend.server = httptest.NewServer(http.HandlerFunc(backend.serveHTTP))
	t.Cleanup(backend.server.Close)
	return backend
}

func (b *bookwarehouseFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Method != http.MethodGet || r.Header.Get("X-API-Key") != fixtureAPIKey {
		http.Error(w, "unauthorized fixture request", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/warehouse/api/v1/monitoring" {
		b.monitoring++
		writeJSON(w, map[string]any{"requests": []any{}, "total": 0, "page": 1, "limit": 100})
		return
	}
	const prefix = "/warehouse/api/v1/books"
	if r.URL.Path == prefix+"/folders" {
		folder := r.URL.Query().Get("folder")
		dirs := []map[string]string{}
		switch folder {
		case "":
			dirs = append(dirs, map[string]string{"name": "private", "path": "private"})
		case "private":
			dirs = append(dirs, map[string]string{"name": "Archive", "path": "private/Archive"})
		}
		writeJSON(w, map[string]any{"path": folder, "exists": folder == "" || folder == "private" || folder == "private/Archive", "directories": dirs, "next_after": ""})
		return
	}
	if r.URL.Path == prefix {
		query := r.URL.Query()
		page, pageErr := strconv.Atoi(query.Get("page"))
		limit, limitErr := strconv.Atoi(query.Get("limit"))
		if pageErr != nil || limitErr != nil || page < 1 || limit < 1 || limit > 100 ||
			query.Get("sort") != "updated_at" || query.Get("order") != "asc" ||
			(query.Get("format") != "epub" && query.Get("format") != "pdf") {
			http.Error(w, "invalid listing parameters", http.StatusBadRequest)
			return
		}
		var matching []map[string]any
		for i, book := range b.books {
			if book.format == query.Get("format") && (query.Get("folder") == "" || strings.HasPrefix(b.metadata(i)["storage_path"].(string), query.Get("folder")+"/")) {
				matching = append(matching, b.metadata(i))
			}
		}
		start := min((page-1)*limit, len(matching))
		end := min(start+limit, len(matching))
		result := matching[start:end]
		if result == nil {
			result = []map[string]any{}
		}
		writeJSON(w, map[string]any{
			"books": result,
			"pagination": map[string]any{
				"page": page, "limit": limit, "total_items": len(matching),
				"total_pages": (len(matching) + limit - 1) / limit,
			},
		})
		return
	}
	for i, book := range b.books {
		if r.URL.Path == prefix+"/"+book.id {
			if b.mode == "metadata-denied" {
				http.Error(w, "fixture denial", http.StatusForbidden)
				return
			}
			writeJSON(w, b.metadata(i))
			return
		}
		if r.URL.Path == prefix+"/"+book.id+"/download" {
			if b.mode == "download-denied" {
				http.Error(w, "fixture denial", http.StatusForbidden)
				return
			}
			if r.Header.Get("Range") == "bytes=0-0" {
				// Download authorization probe on a cache hit.
				b.probes[book.id]++
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(book.body)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(book.body[:1])
				return
			}
			b.downloads[book.id]++
			if r.Header.Get("Range") != "" {
				http.Error(w, "verification requires a full download", http.StatusBadRequest)
				return
			}
			body := book.body
			if b.mode == "corrupt" {
				body = bytes.Repeat([]byte{'X'}, len(body))
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
	}
	http.NotFound(w, r)
}

func (b *bookwarehouseFixture) metadata(index int) map[string]any {
	book := b.books[index]
	hash := strings.TrimPrefix(revision(book.body), "sha256:")
	if b.mode == "changed" {
		hash = strings.Repeat("f", 64)
	}
	return map[string]any{
		"id": book.id, "cover_id": "cover-" + book.id, "title": "Fixture " + book.id,
		"file_format": book.format, "file_size": len(book.body), "file_hash": hash,
		"language": "en", "storage_path": "private/Archive/" + book.id + "." + book.format,
		"created_at": "2026-10-01T00:00:00Z",
		"updated_at": fmt.Sprintf("2026-10-01T00:00:%02dZ", index),
		"has_cover":  false, "enriched": false, "enrichment_attempts": 0,
		"calibre_enriched": false,
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func revision(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
