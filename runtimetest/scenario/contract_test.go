package scenario

import (
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/contract"
)

func TestFixturesConvertToCanonicalContract(t *testing.T) {
	set, err := LoadFile(filepath.Join(testdataDir(t), "scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range set.Scenarios {
		request, err := ToContractRequest(fixture.Request)
		if err != nil {
			t.Fatalf("%s request: %v", fixture.Name, err)
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("%s request validation: %v", fixture.Name, err)
		}
		for _, event := range fixture.Events {
			current, err := ToContractEvent(event)
			if err != nil {
				t.Fatalf("%s event: %v", fixture.Name, err)
			}
			if current.Type == contract.EventModelCompleted && current.Model != nil &&
				current.Model.Result == nil {
				continue
			}
			if err := current.Validate(); err != nil {
				t.Fatalf("%s event validation: %v", fixture.Name, err)
			}
		}
		if fixture.Result != nil {
			if _, err := ToContractResult(*fixture.Result); err != nil {
				t.Fatalf("%s result: %v", fixture.Name, err)
			}
		}
		if fixture.Error != nil {
			if _, err := ToContractError(*fixture.Error); err != nil {
				t.Fatalf("%s error: %v", fixture.Name, err)
			}
		}
	}
}
