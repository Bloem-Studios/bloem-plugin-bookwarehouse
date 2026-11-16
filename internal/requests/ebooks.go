package requests

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// Ebook requests go through Bookwarehouse monitoring (Anna's Archive):
// POST /monitoring/add by ISBN, then GET /monitoring/{id} or /books/{id}.
// This is the request router moved in from bloem.requests.bookwarehouse
// 0.1.5, with the same behavior and the same bwh1 receipts; only the
// connection now comes from the installation.

const maxPages = 100

// cancelledByUser is the error_message Bookwarehouse writes when a
// monitoring request is cancelled; there is no cancelled status.
const cancelledByUser = "Cancelled by user"

type ebookSettings struct {
	allowPDF bool
	format   string
}

// ebookConfig reads the host connection's config: allow_pdf and an optional
// preferred_format overriding the installation's.
func ebookConfig(s Settings, c *pb.RouterConnection) (ebookSettings, map[string]string) {
	out := ebookSettings{format: "epub"}
	if s.PreferredFormat != "" {
		out.format = s.PreferredFormat
	}
	issues := map[string]string{}
	if c == nil {
		issues["connection"] = "A connection is required."
		return out, issues
	}
	fields := c.GetConfig().GetFields()
	if v, ok := fields["allow_pdf"]; ok {
		if _, ok := v.Kind.(*structpb.Value_BoolValue); !ok {
			issues["allow_pdf"] = "Use a boolean value."
		} else {
			out.allowPDF = v.GetBoolValue()
		}
	}
	installationPDF := s.PreferredFormat == "pdf"
	if v, ok := fields["preferred_format"]; ok {
		if _, ok := v.Kind.(*structpb.Value_StringValue); !ok {
			issues["preferred_format"] = "Choose EPUB or PDF."
		} else {
			out.format = v.GetStringValue()
			installationPDF = false
		}
	}
	if out.format != "epub" && (out.format != "pdf" || !out.allowPDF && !installationPDF) {
		issues["preferred_format"] = "PDF requires the connection's Allow PDF setting; otherwise choose EPUB."
	}
	if !validID(c.Id) {
		issues["id"] = "A connection ID is required."
	}
	return out, issues
}

func openEbook(s Settings, c *pb.RouterConnection) (*client, ebookSettings, error) {
	e, issues := ebookConfig(s, c)
	if len(issues) > 0 {
		return nil, e, status.Error(codes.InvalidArgument, "Invalid Bookwarehouse connection configuration.")
	}
	cl, err := newClient(s.BaseURL, s.APIKey, s.AllowHTTP, 30*time.Second)
	if err != nil {
		return nil, e, status.Error(codes.FailedPrecondition, "The installation's Bookwarehouse address or API key is not usable.")
	}
	return cl, e, nil
}

func ebookIdentity(r *pb.RequestDescriptor) (string, string, error) {
	if r == nil || r.MediaType != "book" || len(r.Seasons) != 0 {
		return "", "", status.Error(codes.InvalidArgument, "Only ISBN ebook requests are supported.")
	}
	isbn := r.ExternalIds["isbn"]
	id := r.ExternalIds["bloem_request_id"]
	if !validISBN13(isbn) || !validID(id) {
		return "", "", status.Error(codes.InvalidArgument, "A valid canonical ISBN13 and stable Bloem request ID are required.")
	}
	return isbn, id, nil
}

func validISBN13(s string) bool {
	if len(s) != 13 || (!strings.HasPrefix(s, "978") && !strings.HasPrefix(s, "979")) {
		return false
	}
	sum := 0
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		weight := 1
		if i%2 == 1 {
			weight = 3
		}
		sum += int(s[i]-'0') * weight
	}
	return sum%10 == 0
}

func isbn10To13(s string) string {
	if len(s) != 10 {
		return ""
	}
	sum := 0
	for i := range s {
		v := int(s[i] - '0')
		if i == 9 && s[i] == 'X' {
			v = 10
		} else if s[i] < '0' || s[i] > '9' {
			return ""
		}
		sum += (10 - i) * v
	}
	if sum%11 != 0 {
		return ""
	}
	out := "978" + s[:9]
	sum = 0
	for i := range out {
		weight := 1
		if i%2 == 1 {
			weight = 3
		}
		sum += int(out[i]-'0') * weight
	}
	return out + string(byte('0'+(10-sum%10)%10))
}

// receiptData is the bwh1 receipt. Field names and order form the
// documented deterministic host recovery receipt.
type receiptData struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	ISBN      string `json:"isbn"`
	Format    string `json:"format"`
	RequestID string `json:"request_id"`
}

func encodeReceipt(r receiptData) string {
	b, _ := json.Marshal(r)
	return "bwh1." + base64.RawURLEncoding.EncodeToString(b)
}

func decodeReceipt(raw string) (receiptData, error) {
	var out receiptData
	if len(raw) > 2048 || !strings.HasPrefix(raw, "bwh1.") {
		return out, errBackend
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "bwh1."))
	if err != nil || json.Unmarshal(b, &out) != nil || !validISBN13(out.ISBN) || !validID(out.RequestID) || (out.Format != "epub" && out.Format != "pdf") {
		return out, errBackend
	}
	switch out.Kind {
	case "monitoring", "book":
		if !validID(out.ID) {
			return out, errBackend
		}
	case "presence", "unknown":
		if out.ID != "" {
			return out, errBackend
		}
	default:
		return out, errBackend
	}
	return out, nil
}

// normalizeEbook maps a Bookwarehouse monitoring status (or this plugin's
// own markers) to the host status and a plain message.
func normalizeEbook(raw string) (string, string) {
	switch raw {
	case "completed":
		return "completed", "Downloaded; awaiting linked library scan."
	case "downloading", "importing", "processing", "grabbed":
		return "downloading", "Bookwarehouse is downloading the ebook."
	case "pending", "queued", "searching", "searching_now", "found", "found_pending", "monitored", "retrying", "not_found":
		return "queued", "Bookwarehouse is searching for the ebook."
	case "cancelled", "canceled":
		return "failed", "The request was cancelled in Bookwarehouse."
	case "failed", "expired":
		return "failed", "Bookwarehouse could not complete this request."
	case "unknown":
		return "queued", "Submission outcome unknown; awaiting backend evidence."
	case "book_exists":
		return "completed", "Book already present; awaiting linked library scan."
	default:
		return "failed", "Bookwarehouse returned an unsupported request state."
	}
}

func safeExternal(raw string) string {
	switch raw {
	case "completed", "downloading", "importing", "processing", "grabbed", "pending", "queued", "searching", "searching_now", "found", "found_pending", "monitored", "retrying", "not_found", "failed", "cancelled", "canceled", "expired", "unknown", "book_exists", "missing":
		return raw
	default:
		return "unsupported"
	}
}

func ebookTarget(c *pb.RouterConnection, receipt receiptData, raw string) *pb.FulfillmentTarget {
	state, message := normalizeEbook(raw)
	return &pb.FulfillmentTarget{ConnectionId: c.Id, Quality: receipt.Format, ExternalId: encodeReceipt(receipt), ExternalStatus: safeExternal(raw), Status: state, Message: message}
}

func (r *Router) fulfillEbook(ctx context.Context, s Settings, in *pb.FulfillRequest) (*pb.FulfillResponse, error) {
	isbn, id, err := ebookIdentity(in.Request)
	if err != nil {
		return nil, err
	}
	if len(in.Connections) != 1 {
		return nil, status.Error(codes.InvalidArgument, "Exactly one Bookwarehouse connection is required.")
	}
	c := in.Connections[0]
	cl, e, err := openEbook(s, c)
	if err != nil {
		return nil, err
	}
	defer cl.close()
	if len(in.Qualities) > 1 || (len(in.Qualities) == 1 && (in.Qualities[0].GetId() != e.format || in.Qualities[0].GetIs4K())) {
		return nil, status.Error(codes.InvalidArgument, "Requested format must match the connection configuration.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	row, kind, err := cl.preflight(ctx, isbn, e.format)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "Bookwarehouse preflight unavailable; nothing submitted.")
	}
	receipt := receiptData{Kind: kind, ID: row.ID, ISBN: isbn, Format: e.format, RequestID: id}
	raw := row.state()
	if kind == "" {
		receipt.Kind = "unknown"
		var response struct {
			ID     string `json:"request_id"`
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		code, err := cl.json(ctx, "POST", "/monitoring/add", nil, map[string]string{"isbn": isbn, "preferred_format": e.format}, &response)
		switch {
		case err == nil && code == 202 && validID(response.ID):
			receipt.Kind = "monitoring"
			receipt.ID = response.ID
			raw = "searching"
		case err == nil && code == 409 && response.Error == "book_exists":
			receipt.Kind = "presence"
			raw = "book_exists"
		case code >= 400 && code < 500 && code != 409:
			return nil, status.Error(codes.FailedPrecondition, "Bookwarehouse rejected the ebook submission.")
		default:
			raw = "unknown"
		}
	}
	return &pb.FulfillResponse{Targets: []*pb.FulfillmentTarget{ebookTarget(c, receipt, raw)}}, nil
}

func (r *Router) checkEbook(ctx context.Context, s Settings, in *pb.CheckStatusRequest) (*pb.CheckStatusResponse, error) {
	isbn, id, err := ebookIdentity(in.Request)
	if err != nil {
		return nil, err
	}
	if len(in.Connections) != 1 || len(in.Targets) != 1 {
		return nil, status.Error(codes.InvalidArgument, "Exactly one Bookwarehouse connection and receipt are required.")
	}
	c := in.Connections[0]
	ref := in.Targets[0]
	receipt, err := decodeReceipt(ref.GetExternalId())
	if err != nil || receipt.ISBN != isbn || receipt.RequestID != id || c == nil || ref.ConnectionId != c.Id || ref.Quality != receipt.Format {
		return nil, status.Error(codes.InvalidArgument, "Receipt does not match the request and connection.")
	}
	cl, e, err := openEbook(s, c)
	if err != nil {
		return nil, err
	}
	defer cl.close()
	if receipt.Format != e.format {
		return nil, status.Error(codes.FailedPrecondition, "Connection format differs from the durable receipt.")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw := "unknown"
	switch receipt.Kind {
	case "unknown", "presence":
		row, kind, err := cl.preflight(ctx, isbn, receipt.Format)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "Bookwarehouse status unavailable.")
		}
		if kind != "" {
			raw = row.state()
		} else if receipt.Kind == "presence" {
			raw = "book_exists"
		}
	case "monitoring", "book":
		var row backendRow
		path := "/monitoring/"
		book := receipt.Kind == "book"
		if book {
			path = "/books/"
		}
		code, err := cl.json(ctx, "GET", path+receipt.ID, nil, nil, &row)
		if code == 404 {
			raw = "missing"
		} else if err != nil || code != 200 {
			return nil, status.Error(codes.Unavailable, "Bookwarehouse status unavailable.")
		} else if row.ID != receipt.ID || !row.matches(isbn, receipt.Format, book) {
			raw = "unsupported"
		} else {
			raw = row.state()
			if book {
				raw = "completed"
			}
		}
	}
	state, message := normalizeEbook(raw)
	if raw == "missing" {
		state = "failed"
		message = "Backend receipt missing; review required before any retry."
	}
	return &pb.CheckStatusResponse{Statuses: []*pb.TargetStatus{{ConnectionId: c.Id, Quality: receipt.Format, ExternalStatus: safeExternal(raw), Status: state, Message: message}}}, nil
}

func validateEbook(s Settings, in *pb.ValidateRequest) *pb.ValidateResponse {
	_, issues := ebookConfig(s, in.Connection)
	out := &pb.ValidateResponse{FieldErrors: issues}
	if len(in.Siblings) > 0 {
		out.FormError = "Only one Bookwarehouse connection is supported."
	}
	return out
}

func ebookOptions(s Settings, c *pb.RouterConnection) *pb.ListConfigOptionsResponse {
	options := []*pb.ConfigOption{{Value: "epub", Label: "EPUB"}}
	e, _ := ebookConfig(s, c)
	if e.allowPDF || s.PreferredFormat == "pdf" {
		options = append(options, &pb.ConfigOption{Value: "pdf", Label: "PDF"})
	}
	return &pb.ListConfigOptionsResponse{OptionsByField: map[string]*pb.ConfigOptionList{"preferred_format": {Options: options}}}
}

func testEbook(ctx context.Context, s Settings) *pb.TestConnectionResponse {
	cl, err := newClient(s.BaseURL, s.APIKey, s.AllowHTTP, 30*time.Second)
	if err != nil {
		return &pb.TestConnectionResponse{Message: "Invalid Bookwarehouse connection configuration."}
	}
	defer cl.close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := cl.monitoring(ctx, 1); err != nil {
		return &pb.TestConnectionResponse{Message: "Bookwarehouse monitoring authentication or response check failed."}
	}
	return &pb.TestConnectionResponse{Ok: true, Message: "Bookwarehouse monitoring API authenticated."}
}

type backendRow struct {
	ID             string `json:"id"`
	ISBN13         string `json:"isbn13"`
	ISBN10         string `json:"isbn10"`
	Status         string `json:"status"`
	Format         string `json:"format"`
	SelectedFormat string `json:"selected_format"`
	FileFormat     string `json:"file_format"`
	ErrorMessage   string `json:"error_message"`
}

// state is the row's status, reading a cancellation (failed with
// "Cancelled by user") as cancelled.
func (r backendRow) state() string {
	if r.Status == "failed" && r.ErrorMessage == cancelledByUser {
		return "cancelled"
	}
	return r.Status
}

func (r backendRow) matches(isbn, format string, book bool) bool {
	actual := r.SelectedFormat
	if actual == "" {
		actual = r.Format
	}
	if book {
		actual = r.FileFormat
	}
	return (r.ISBN13 == isbn || isbn10To13(r.ISBN10) == isbn) && actual == format
}

type monitoringPage struct {
	Rows  []backendRow `json:"requests"`
	Total *int         `json:"total"`
	Page  *int         `json:"page"`
	Limit *int         `json:"limit"`
}

func (c *client) monitoring(ctx context.Context, page int) (monitoringPage, error) {
	var p monitoringPage
	code, err := c.json(ctx, http.MethodGet, "/monitoring", url.Values{"page": {strconv.Itoa(page)}, "limit": {"100"}}, nil, &p)
	if err != nil || code != 200 || p.Rows == nil || p.Total == nil || p.Page == nil || p.Limit == nil || *p.Page != page || *p.Limit != 100 || *p.Total < 0 || *p.Total > maxPages*100 || len(p.Rows) > 100 {
		return p, errBackend
	}
	expected := min(100, max(0, *p.Total-(page-1)*100))
	if len(p.Rows) != expected {
		return p, errBackend
	}
	return p, nil
}

// preflight looks before ordering: a monitoring row or a book with this
// ISBN and format means nothing is submitted.
func (c *client) preflight(ctx context.Context, isbn, format string) (backendRow, string, error) {
	var match backendRow
	total := -1
	seen := map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		p, err := c.monitoring(ctx, page)
		if err != nil {
			return backendRow{}, "", err
		}
		if total >= 0 && total != *p.Total {
			return backendRow{}, "", errBackend
		}
		total = *p.Total
		for _, row := range p.Rows {
			if !validID(row.ID) || seen[row.ID] {
				return backendRow{}, "", errBackend
			}
			seen[row.ID] = true
			if row.matches(isbn, format, false) {
				if match.ID != "" && match.ID != row.ID {
					return backendRow{}, "", errBackend
				}
				match = row
			}
		}
		if page*100 >= total {
			break
		}
	}
	// Prefer actual book presence over monitoring state; provider presence is never host fulfillment.
	total = -1
	seen = map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		var p struct {
			Books      []backendRow `json:"books"`
			Pagination *struct {
				Page  int `json:"page"`
				Limit int `json:"limit"`
				Total int `json:"total_items"`
				Pages int `json:"total_pages"`
			} `json:"pagination"`
		}
		code, err := c.json(ctx, http.MethodGet, "/books", url.Values{"page": {strconv.Itoa(page)}, "limit": {"100"}, "format": {format}, "sort": {"updated_at"}, "order": {"asc"}}, nil, &p)
		if err != nil || code != 200 || p.Books == nil || p.Pagination == nil {
			return backendRow{}, "", errBackend
		}
		q := p.Pagination
		pages := q.Total / 100
		if q.Total%100 != 0 {
			pages++
		}
		if q.Page != page || q.Limit != 100 || q.Total < 0 || q.Pages != pages || len(p.Books) != min(100, max(0, q.Total-(page-1)*100)) || (total >= 0 && q.Total != total) {
			return backendRow{}, "", errBackend
		}
		total = q.Total
		for _, row := range p.Books {
			if !validID(row.ID) || seen[row.ID] {
				return backendRow{}, "", errBackend
			}
			seen[row.ID] = true
			if row.matches(isbn, format, true) {
				row.Status = "completed"
				return row, "book", nil
			}
		}
		// Book presence is an optimization, not a required global inventory.
		// On large catalogs, the provider's atomic book_exists response to add
		// supplies presence evidence without scanning hundreds of thousands of
		// books. Monitoring preflight still completes before any submission.
		if total > maxPages*100 || page*100 >= total {
			break
		}
	}
	if match.ID != "" {
		return match, "monitoring", nil
	}
	return backendRow{}, "", nil
}
