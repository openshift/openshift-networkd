package vnid

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/openshift/openshift-sdn/pkg/netid"
)

type VNIDRange struct {
	Base uint
	Size uint
}

// Contains tests whether a given vnid falls within the Range.
func (r *VNIDRange) Contains(vnid uint) (bool, uint) {
	if (vnid >= r.Base) && ((vnid - r.Base) < r.Size) {
		offset := vnid - r.Base
		return true, offset
	}
	return false, 0
}

func (r *VNIDRange) String() string {
	if r.Size == 0 {
		return ""
	}
	return fmt.Sprintf("%d-%d", r.Base, r.Base+r.Size-1)
}

func (r *VNIDRange) Set(base, size uint) error {
	if base < netid.MinVNID {
		return fmt.Errorf("invalid vnid base, must be greater than %d", netid.MinVNID)
	}
	if size == 0 {
		return fmt.Errorf("invalid vnid size, must be greater than zero")
	}
	if (base + size - 1) > netid.MaxVNID {
		return fmt.Errorf("vnid range exceeded max value %d", netid.MaxVNID)
	}

	r.Base = base
	r.Size = size
	return nil
}

func NewVNIDRange(base, size uint) (*VNIDRange, error) {
	r := &VNIDRange{}
	err := r.Set(base, size)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Parse range string of the form "min-max", inclusive at both ends
// and returns VNIDRange object.
func ParseVNIDRange(value string) (*VNIDRange, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("invalid range string")
	}

	hyphenIndex := strings.Index(value, "-")
	if hyphenIndex == -1 {
		return nil, fmt.Errorf("expected hyphen in port range")
	}

	var err error
	var low, high int
	low, err = strconv.Atoi(value[:hyphenIndex])
	if err == nil {
		high, err = strconv.Atoi(value[hyphenIndex+1:])
	}
	if err != nil {
		return nil, fmt.Errorf("unable to parse vnid range: %s", value)
	}
	return NewVNIDRange(uint(low), uint(high-low+1))
}
