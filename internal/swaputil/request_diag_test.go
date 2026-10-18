package swaputil

import (
	"context"
	"testing"
)

func TestPublishRequestProviderNotifiesRouteReporter(t *testing.T) {
	type route struct{ provider, adapter string }
	var reported []route

	ctx := WithRequestDiagnostics(context.Background())
	ctx = WithInferenceRouteReporter(ctx, func(provider, adapter string) {
		reported = append(reported, route{provider, adapter})
	})

	PublishRequestProvider(ctx, "opencode-go", "Claude")
	if len(reported) != 1 || reported[0] != (route{"opencode-go", "Claude"}) {
		t.Fatalf("reported = %+v, want one opencode-go/Claude route", reported)
	}

	// First publisher wins: a request that failed over keeps the route that
	// answered, and the live row must not be rewritten by a later attempt.
	PublishRequestProvider(ctx, "CPA", "OpenAI")
	if len(reported) != 1 {
		t.Fatalf("reported = %+v, want the losing route to stay unreported", reported)
	}

	// A locally hosted model publishes nothing, so the reporter stays silent
	// and the row keeps reading as local.
	PublishRequestProvider(ctx, "   ", "Chat")
	if len(reported) != 1 {
		t.Fatalf("reported = %+v, want blank provider to stay unreported", reported)
	}
}

func TestReportInferenceAttemptDoesNotAttributeTheRecord(t *testing.T) {
	type route struct{ provider, adapter string }
	var reported []route

	ctx := WithRequestDiagnostics(context.Background())
	ctx = WithInferenceRouteReporter(ctx, func(provider, adapter string) {
		reported = append(reported, route{provider, adapter})
	})

	// Attempt reporting is live-only: a failover must be visible in the
	// in-flight row while it happens, without becoming the provider the
	// finished record claims served the request.
	ReportInferenceAttempt(ctx, "opencode-go", "Claude")
	ReportInferenceAttempt(ctx, "CPA", "OpenAI")
	ReportInferenceAttempt(ctx, "   ", "")
	if len(reported) != 2 {
		t.Fatalf("reported = %+v, want both attempts and no blank route", reported)
	}

	diagnostics := RequestDiagnosticsFrom(ctx)
	if diagnostics.Provider != "" {
		t.Fatalf("record provider = %q, want the record left for the answering route", diagnostics.Provider)
	}
}

func TestReportLocalRouteResolvesToNoProvider(t *testing.T) {
	calls := 0
	var provider, adapter string

	ctx := WithInferenceRouteReporter(context.Background(), func(p, a string) {
		calls++
		provider, adapter = p, a
	})

	ReportLocalRoute(ctx)
	if calls != 1 || provider != "" || adapter != "" {
		t.Fatalf("reported %d time(s) as %q/%q, want one empty route", calls, provider, adapter)
	}
}

func TestPublishRequestProviderWithoutReporter(t *testing.T) {
	ctx := WithRequestDiagnostics(context.Background())

	// No reporter attached: publishing must still record the holder value and
	// must not panic, which is the path embedders without in-flight tracking
	// take.
	PublishRequestProvider(ctx, "opencode-go", "Chat")

	diagnostics := RequestDiagnosticsFrom(ctx)
	if diagnostics == nil || diagnostics.Provider != "opencode-go" || diagnostics.Adapter != "Chat" {
		t.Fatalf("diagnostics = %+v, want opencode-go/Chat", diagnostics)
	}
}

func TestPublishLocalRouteMarksTheRecordLocal(t *testing.T) {
	ctx := WithRequestDiagnostics(context.Background())

	PublishLocalRoute(ctx)
	if diagnostics := RequestDiagnosticsFrom(ctx); !diagnostics.Local || diagnostics.Provider != "" {
		t.Fatalf("diagnostics = %+v, want a local row", diagnostics)
	}

	// A locally served request never reaches the gateway, so a later provider
	// publish must not rewrite it.
	PublishRequestProvider(ctx, "opencode-go", "Chat")
	if diagnostics := RequestDiagnosticsFrom(ctx); !diagnostics.Local || diagnostics.Provider != "" {
		t.Fatalf("diagnostics = %+v, want the local marker kept", diagnostics)
	}
}

func TestPublishLocalRouteLeavesAProviderRequestAlone(t *testing.T) {
	ctx := WithRequestDiagnostics(context.Background())

	// The other order: the request went to a provider, so it must not be
	// reported as local (and the provider attribution stays).
	PublishRequestProvider(ctx, "opencode-go", "Chat")
	PublishLocalRoute(ctx)

	diagnostics := RequestDiagnosticsFrom(ctx)
	if diagnostics.Local {
		t.Fatalf("diagnostics = %+v, want no local marker on a provider request", diagnostics)
	}
	if diagnostics.Provider != "opencode-go" {
		t.Fatalf("provider = %q, want opencode-go", diagnostics.Provider)
	}
}

func TestUnattributedRequestStaysUnknown(t *testing.T) {
	// A provider request that failed before any upstream answered publishes
	// neither a provider nor a local marker: the record must stay unattributed
	// rather than claiming the request ran here.
	ctx := WithRequestDiagnostics(context.Background())
	diagnostics := RequestDiagnosticsFrom(ctx)
	if diagnostics.Local || diagnostics.Provider != "" {
		t.Fatalf("diagnostics = %+v, want an unattributed row", diagnostics)
	}
}
