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
