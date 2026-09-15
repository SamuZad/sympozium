package helmchart

import "testing"

func TestVendoredErgozChartMatchesItsPin(t *testing.T) {
	ch, pin, err := LoadErgoz()
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name() != "ergoz" || "v"+ch.Metadata.Version != pin.Version {
		t.Fatalf("chart %s %s does not match pin %+v", ch.Name(), ch.Metadata.Version, pin)
	}
}
