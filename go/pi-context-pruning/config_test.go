package picontextpruning

import (
	"math"
	"reflect"
	"testing"
)

// TestValidGlobalAndProjectFieldsMergeInOrder ports:
// "valid global and project fields merge in order" from test/config.test.ts
func TestValidGlobalAndProjectFieldsMergeInOrder(t *testing.T) {
	global := MergeConfig(DEFAULT_CONFIG, map[string]any{
		"contextPruning": map[string]any{
			"pruneMinimum":   5000,
			"protectedTools": []string{"read"},
		},
	})

	project := MergeConfig(global, map[string]any{
		"contextPruning": map[string]any{
			"enabled":        false,
			"protectedTurns": 3,
		},
	})

	if project.PruneMinimum != 5000 {
		t.Fatalf("expected PruneMinimum == 5000, got %d", project.PruneMinimum)
	}
	expectedTools := []string{"read"}
	if !reflect.DeepEqual(project.ProtectedTools, expectedTools) {
		t.Fatalf("expected ProtectedTools == %v, got %v", expectedTools, project.ProtectedTools)
	}
	if project.Enabled != false {
		t.Fatalf("expected Enabled == false, got %v", project.Enabled)
	}
	if project.ProtectedTurns != 3 {
		t.Fatalf("expected ProtectedTurns == 3, got %d", project.ProtectedTurns)
	}
}

// TestInvalidProjectValuesPreserveValidGlobalValues ports:
// "invalid project values preserve valid global values" from test/config.test.ts
func TestInvalidProjectValuesPreserveValidGlobalValues(t *testing.T) {
	global := MergeConfig(DEFAULT_CONFIG, map[string]any{
		"contextPruning": map[string]any{
			"pruneMinimum":   7000,
			"protectedTools": []string{"read"},
		},
	})

	invalid := map[string]any{
		"contextPruning": map[string]any{
			"enabled":        "false",          // invalid type: string instead of bool
			"pruneMinimum":   -1,               // invalid integer: negative
			"protectedTools": []any{"read", 1}, // invalid string array: contains number
		},
	}

	result := MergeConfig(global, invalid)
	if result.Enabled != true {
		t.Fatalf("expected Enabled == true (preserved), got %v", result.Enabled)
	}
	if result.PruneMinimum != 7000 {
		t.Fatalf("expected PruneMinimum == 7000 (preserved), got %d", result.PruneMinimum)
	}
	expectedTools := []string{"read"}
	if !reflect.DeepEqual(result.ProtectedTools, expectedTools) {
		t.Fatalf("expected ProtectedTools == %v (preserved), got %v", expectedTools, result.ProtectedTools)
	}
}

func TestValidationHelpers(t *testing.T) {
	// ValidInteger
	if v, ok := ValidInteger(10); !ok || v != 10 {
		t.Fatalf("expected 10 true, got %v %v", v, ok)
	}
	if v, ok := ValidInteger(float64(42)); !ok || v != 42 {
		t.Fatalf("expected 42 true, got %v %v", v, ok)
	}
	if _, ok := ValidInteger(float64(42.5)); ok {
		t.Fatalf("expected non-integer float to fail")
	}
	if _, ok := ValidInteger(-5); ok {
		t.Fatalf("expected negative integer to fail")
	}
	if _, ok := ValidInteger("not a number"); ok {
		t.Fatalf("expected string to fail ValidInteger")
	}

	// ValidStrings
	if s, ok := ValidStrings([]string{"a", "b"}); !ok || len(s) != 2 {
		t.Fatalf("expected []string to pass")
	}
	if s, ok := ValidStrings([]any{"a", "b"}); !ok || len(s) != 2 {
		t.Fatalf("expected []any with strings to pass")
	}
	if _, ok := ValidStrings([]any{"a", 123}); ok {
		t.Fatalf("expected mixed []any to fail ValidStrings")
	}
	if _, ok := ValidStrings("not an array"); ok {
		t.Fatalf("expected string to fail ValidStrings")
	}
}

func TestValidIntegerOverflowCap(t *testing.T) {
	// 1e19 exceeds int64/float safe range and should cap at math.MaxInt without overflowing to negative.
	v, ok := ValidInteger(1e19)
	if !ok {
		t.Fatalf("expected 1e19 to be recognized as valid integer")
	}
	if v < 0 {
		t.Fatalf("expected 1e19 not to overflow into negative value, got %d", v)
	}
	if v != math.MaxInt {
		t.Fatalf("expected 1e19 to cap at math.MaxInt (%d), got %d", math.MaxInt, v)
	}

	// MergeConfig with 1e19
	cfg := MergeConfig(DEFAULT_CONFIG, map[string]any{
		"contextPruning": map[string]any{
			"pruneMinimum": 1e19,
		},
	})
	if cfg.PruneMinimum != math.MaxInt {
		t.Fatalf("expected PruneMinimum == math.MaxInt, got %d", cfg.PruneMinimum)
	}

	// Pruning behavior: never prunes when configured with 1e19
	messages := makeOldHistory([]string{"read", "bash"})
	_, stats := PruneToolOutputs(messages, cfg)
	if stats.MessagesPruned != 0 {
		t.Fatalf("expected 0 messages pruned with 1e19 pruneMinimum, got %d", stats.MessagesPruned)
	}
}
