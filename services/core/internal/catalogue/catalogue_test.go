package catalogue_test

import (
	"strings"
	"testing"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/catalogue"
)

func TestLoadAndValidateInput(t *testing.T) {
	cat, err := catalogue.Load("../../../../tools")
	if err != nil {
		t.Fatal(err)
	}
	create := cat.Tools["application.create"]
	if create == nil {
		t.Fatal("application.create missing")
	}
	ok := map[string]any{"product_code": "SME-WC", "requested_amount_minor": 3000000, "currency": "USD", "requested_term_months": 12}
	if err := create.ValidateInput(ok); err != nil {
		t.Errorf("valid input rejected: %v", err)
	}
	bad := map[string]any{"product_code": "SME-WC", "requested_amount_minor": -5, "currency": "usd"}
	err = create.ValidateInput(bad)
	if err == nil {
		t.Fatal("invalid input accepted")
	}
	for _, want := range []string{"requested_amount_minor", "currency", "requested_term_months"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	get := cat.Tools["application.get"]
	if err := get.ValidateInput(map[string]any{"application_id": "not-a-uuid"}); err == nil {
		t.Error("format: uuid is not enforced")
	}
}
