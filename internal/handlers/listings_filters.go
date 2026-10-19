package handlers

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/tecisage/resell-apis/internal/dtos"
)

const MaxCityLength = 100

// Filter is a type alias to dtos.ListingFilter to allow seamless sharing with service layer
type Filter = dtos.ListingFilter

// FilterRule represents a single decomposed filter condition (e.g. for dynamic SQL or rule arrays)
type FilterRule struct {
	FilterName string `json:"filter_name"`
	Op         string `json:"op"`
	Value      string `json:"value"`
}

// parseListingsFilters extracts and validates filter criteria from query parameters
func parseListingsFilters(params url.Values) (Filter, error) {
	var f Filter

	// 1. Status (default: 'ready')
	status := strings.ToLower(strings.TrimSpace(params.Get("status")))
	if status == "" {
		f.Status = string(dtos.ListingStatusReady)
	} else {
		switch status {
		case string(dtos.ListingStatusReady),
			string(dtos.ListingStatusProcessing),
			string(dtos.ListingStatusRejected),
			"all":
			f.Status = status
		default:
			return Filter{}, &dtos.ValidationError{
				Field:   "status",
				Message: fmt.Sprintf("invalid status filter: %q (allowed: ready, processing, rejected, all)", status),
			}
		}
	}

	// 2. City
	if city := strings.TrimSpace(params.Get("city")); city != "" {
		if len(city) > MaxCityLength {
			return Filter{}, &dtos.ValidationError{
				Field:   "city",
				Message: fmt.Sprintf("city cannot be longer than %d characters", MaxCityLength),
			}
		}
		f.City = city
	}

	// 3. PriceFrom (supports both price_from and min_price)
	priceFromRaw := params.Get("price_from")
	if priceFromRaw == "" {
		priceFromRaw = params.Get("min_price")
	}
	if priceFromRaw != "" {
		priceFrom, err := strconv.ParseInt(priceFromRaw, 10, 64)
		if err != nil {
			return Filter{}, &dtos.ValidationError{
				Field:   "price_from",
				Message: "price_from must be an integer value",
			}
		}
		if priceFrom < 0 {
			return Filter{}, &dtos.ValidationError{
				Field:   "price_from",
				Message: "price_from must be greater than or equal to 0",
			}
		}
		f.PriceFrom = &priceFrom
	}

	// 4. PriceTo (supports both price_to and max_price)
	priceToRaw := params.Get("price_to")
	if priceToRaw == "" {
		priceToRaw = params.Get("max_price")
	}
	if priceToRaw != "" {
		priceTo, err := strconv.ParseInt(priceToRaw, 10, 64)
		if err != nil {
			return Filter{}, &dtos.ValidationError{
				Field:   "price_to",
				Message: "price_to must be an integer value",
			}
		}
		if priceTo < 0 {
			return Filter{}, &dtos.ValidationError{
				Field:   "price_to",
				Message: "price_to must be greater than or equal to 0",
			}
		}
		f.PriceTo = &priceTo
	}

	// 5. Cross-field validation: price_from cannot exceed price_to
	if f.PriceFrom != nil && f.PriceTo != nil && *f.PriceFrom > *f.PriceTo {
		return Filter{}, &dtos.ValidationError{
			Field:   "price_from",
			Message: fmt.Sprintf("price_from (%d) cannot be greater than price_to (%d)", *f.PriceFrom, *f.PriceTo),
		}
	}

	return f, nil
}

// ToRules converts the strongly-typed Filter into a slice of FilterRule items
func ToRules(f Filter) []FilterRule {
	var rules []FilterRule

	if f.Status != "" && f.Status != "all" {
		rules = append(rules, FilterRule{
			FilterName: "status",
			Op:         "eq",
			Value:      f.Status,
		})
	}

	if f.City != "" {
		rules = append(rules, FilterRule{
			FilterName: "city",
			Op:         "eq",
			Value:      f.City,
		})
	}

	if f.PriceFrom != nil {
		rules = append(rules, FilterRule{
			FilterName: "price",
			Op:         "gte",
			Value:      strconv.FormatInt(dtos.RupeesToPaise(*f.PriceFrom), 10),
		})
	}

	if f.PriceTo != nil {
		rules = append(rules, FilterRule{
			FilterName: "price",
			Op:         "lte",
			Value:      strconv.FormatInt(dtos.RupeesToPaise(*f.PriceTo), 10),
		})
	}

	return rules
}
