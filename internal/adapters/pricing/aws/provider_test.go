package aws

import "testing"

func TestRegionName_KnownCodesMapToPricingAPILocationNames(t *testing.T) {
	cases := map[string]string{
		"eu-north-1": "EU (Stockholm)",
		"us-east-1":  "US East (N. Virginia)",
		"eu-west-1":  "EU (Ireland)",
		"ap-south-1": "Asia Pacific (Mumbai)",
	}
	for code, want := range cases {
		if got := regionName(code); got != want {
			t.Errorf("regionName(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestRegionName_UnknownCodeFallsThroughUnchanged(t *testing.T) {
	// Documents the current fallback behavior: an unmapped region code is
	// passed to AWS's location filter as-is, which will match zero real
	// products rather than erroring -- this is why every region actually
	// used must have an entry above.
	if got := regionName("xx-made-up-1"); got != "xx-made-up-1" {
		t.Errorf("regionName(unmapped) = %q, want the code unchanged", got)
	}
}
