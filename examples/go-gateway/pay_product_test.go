package main

import "testing"

func TestProductForEngine(t *testing.T) {
	cfg := &PayConfig{StandardProduct: "standard", StandardPrice: 10,
		EnhancedProduct: "enhanced", EnhancedPrice: 30}
	check := func(engine, wantProduct string, wantPrice int) {
		t.Helper()
		if product, price := cfg.productFor(engine); product != wantProduct || price != wantPrice {
			t.Fatalf("%s: got %s/%d, want %s/%d", engine, product, price, wantProduct, wantPrice)
		}
	}
	check("Bing", "standard", 10)
	check("Google", "standard", 10)
	check("GPT-6", "enhanced", 30)
	check("DeepSeek-V4.1-Flash", "enhanced", 30)

	cfg.EngineProducts = map[string]enginePrice{"Google": {"google", 20}, "GPT-6": {"gpt", 40}}
	check("Bing", "standard", 10)
	check("Google", "google", 20)
	check("GPT-6", "gpt", 40)
	check("GPT-5.6", "enhanced", 30)
	check("DeepSeek-V4.1-Flash", "enhanced", 30)
}

func TestLoadPayConfigEngineProducts(t *testing.T) {
	for key, value := range map[string]string{
		"VIRTUALPAY_ENABLED": "true", "VIRTUALPAY_OFFER_ID": "offer", "VIRTUALPAY_APP_KEY": "key",
		"VIRTUALPAY_NOTIFY_TOKEN": "token", "VIRTUALPAY_ENCODING_AES_KEY": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
		"VIRTUALPAY_STANDARD_PRODUCT_ID": "standard", "VIRTUALPAY_STANDARD_PRICE_FEN": "10",
		"VIRTUALPAY_ENHANCED_PRODUCT_ID": "enhanced", "VIRTUALPAY_ENHANCED_PRICE_FEN": "30",
		"VIRTUALPAY_GPT_PRODUCT_ID": "gpt", "VIRTUALPAY_GPT_PRICE_FEN": "40",
	} {
		t.Setenv(key, value)
	}
	cfg, err := loadPayConfig()
	if err != nil {
		t.Fatal(err)
	}
	if product, price := cfg.productFor("GPT-6"); product != "gpt" || price != 40 {
		t.Fatalf("GPT-6: got %s/%d", product, price)
	}

	t.Setenv("VIRTUALPAY_GOOGLE_PRICE_FEN", "20")
	if _, err := loadPayConfig(); err == nil {
		t.Fatal("price without product must be rejected")
	}
}

func TestOrderAmountsIOSRounding(t *testing.T) {
	cfg := &PayConfig{StandardProduct: "standard", StandardPrice: 10,
		EnhancedProduct: "enhanced", EnhancedPrice: 30,
		EngineProducts: map[string]enginePrice{"GPT-6": {"gpt", 40}},
		IOSProduct:     "ios", IOSUnit: 50}
	cases := []struct {
		name, engine                  string
		pages                         int
		ios                           bool
		product                       string
		unit, quantity, total, pagesN int
	}{
		{"android keeps page billing", "Bing", 3, false, "standard", 10, 3, 30, 3},
		{"ios below minimum is raised to 1 yuan", "Bing", 1, true, "ios", 50, 2, 100, 1},
		{"ios exactly 1 yuan", "Bing", 10, true, "ios", 50, 2, 100, 10},
		{"ios rounds up to 0.5 yuan steps", "Bing", 11, true, "ios", 50, 3, 150, 11},
		{"ios llm 0.3 x 5 = 1.5", "DeepSeek-V4.1-Flash", 5, true, "ios", 50, 3, 150, 5},
		{"ios llm 0.3 x 6 = 1.8 -> 2.0", "DeepSeek-V4.1-Flash", 6, true, "ios", 50, 4, 200, 6},
		{"ios gpt 0.4 x 4 = 1.6 -> 2.0", "GPT-6", 4, true, "ios", 50, 4, 200, 4},
	}
	for _, c := range cases {
		product, unit, quantity, total := cfg.orderAmounts(c.engine, c.pages, c.ios)
		if product != c.product || unit != c.unit || quantity != c.quantity || total != c.total {
			t.Errorf("%s: got %s/%d x%d = %d, want %s/%d x%d = %d", c.name,
				product, unit, quantity, total, c.product, c.unit, c.quantity, c.total)
		}
		if total < c.pages*cfgSubtotalUnit(cfg, c.engine) {
			t.Errorf("%s: total %d is below the plain page price", c.name, total)
		}
		if c.ios && total < iosMinFen {
			t.Errorf("%s: iOS total %d is below the minimum", c.name, total)
		}
	}

	cfg.IOSProduct, cfg.IOSUnit = "", 0
	if product, unit, quantity, total := cfg.orderAmounts("Bing", 1, true); product != "standard" || unit != 10 || quantity != 1 || total != 10 {
		t.Fatalf("without an iOS product iOS must fall back, got %s/%d x%d = %d", product, unit, quantity, total)
	}
}

func cfgSubtotalUnit(cfg *PayConfig, engine string) int {
	_, unit := cfg.productFor(engine)
	return unit
}

func TestPayOrderPageCountFallsBackToQuantity(t *testing.T) {
	if got := (PayOrder{Quantity: 7}).PageCount(); got != 7 {
		t.Fatal("orders without Pages must report Quantity, got", got)
	}
	if got := (PayOrder{Pages: 1, Quantity: 2}).PageCount(); got != 1 {
		t.Fatal("iOS orders must report pages, not units, got", got)
	}
}

func TestLoadPayConfigIOSProduct(t *testing.T) {
	for key, value := range map[string]string{
		"VIRTUALPAY_ENABLED": "true", "VIRTUALPAY_OFFER_ID": "offer", "VIRTUALPAY_APP_KEY": "key",
		"VIRTUALPAY_NOTIFY_TOKEN": "token", "VIRTUALPAY_ENCODING_AES_KEY": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
		"VIRTUALPAY_STANDARD_PRODUCT_ID": "standard", "VIRTUALPAY_STANDARD_PRICE_FEN": "10",
		"VIRTUALPAY_ENHANCED_PRODUCT_ID": "enhanced", "VIRTUALPAY_ENHANCED_PRICE_FEN": "30",
	} {
		t.Setenv(key, value)
	}
	if cfg, err := loadPayConfig(); err != nil || cfg.IOSProduct != "" {
		t.Fatal("iOS product is optional", err)
	}
	t.Setenv("VIRTUALPAY_IOS_PRICE_FEN", "50")
	if _, err := loadPayConfig(); err == nil {
		t.Fatal("price without product must be rejected")
	}
	t.Setenv("VIRTUALPAY_IOS_PRODUCT_ID", "ios")
	cfg, err := loadPayConfig()
	if err != nil || cfg.IOSProduct != "ios" || cfg.IOSUnit != 50 {
		t.Fatal("valid iOS product rejected", err)
	}
	t.Setenv("VIRTUALPAY_IOS_PRICE_FEN", "150")
	if _, err := loadPayConfig(); err == nil {
		t.Fatal("a unit above the iOS minimum can never reach exactly the minimum and must be rejected")
	}
}
