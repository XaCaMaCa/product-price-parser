package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMapCityToXDomain(t *testing.T) {
	t.Parallel()
	if got, want := mapCityToXDomain("  "), "moscow"; got != want {
		t.Fatalf("empty: got %q want %q", got, want)
	}
	if got, want := mapCityToXDomain("Санкт-Петербург"), "spb"; got != want {
		t.Fatalf("spb: got %q want %q", got, want)
	}
	if got, want := mapCityToXDomain("уфа"), "ufa"; got != want {
		t.Fatalf("ufa: got %q want %q", got, want)
	}
}

func TestFilterByCategories(t *testing.T) {
	t.Parallel()
	items := []Product{
		{Category: "молочка", Name: "x", Price: "1", URL: "u"},
		{Category: "овощи", Name: "y", Price: "2", URL: "v"},
	}
	out := filterByCategories(items, []string{"молочная продукция"})
	if len(out) != 1 || out[0].Name != "x" {
		t.Fatalf("ожидалась 1 молочная, got %#v", out)
	}
}

func TestUrlTrimJunk(t *testing.T) {
	t.Parallel()
	if g := urlTrimTrailingJunk("/p/tovar|"); g != "/p/tovar" {
		t.Fatalf("got %q", g)
	}
}

// TestOfflineFixturesExtractAndFilter — интеграция loadOffline + extract + filter (golden на fixtures).
func TestOfflineFixturesExtractAndFilter(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	root := filepath.Dir(thisFile)
	fixDir := filepath.Join(root, "fixtures")
	c := cfg{FixturesPath: fixDir, City: "Москва"}
	store, raws, err := loadOffline(c)
	if err != nil {
		t.Fatalf("loadOffline: %v", err)
	}
	if store.ID != 101 {
		t.Fatalf("store id: got %d", store.ID)
	}
	items := extractProducts(raws, 0)
	// 4 товара из home.json + 2 из mobilepromo.json; stores.json без name+price+url
	if got := len(items); got != 6 {
		t.Fatalf("ожидалось 6 товаров из фикстур, got %d", got)
	}
	for _, p := range items {
		if p.URL == "" || !strings.HasPrefix(p.URL, "https://lenta.com") {
			t.Fatalf("ожидался абсолютный url lenta.com, got %q", p.URL)
		}
	}
	cats := []string{"молочная продукция", "хлебобулочные изделия"}
	filtered := filterByCategories(items, cats)
	if got := len(filtered); got != 6 {
		t.Fatalf("после фильтра по двум категориям ожидалось 6, got %d", got)
	}
}
