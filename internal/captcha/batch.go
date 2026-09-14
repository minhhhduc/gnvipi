package captcha

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

// extractBatchHarness fires hcaptcha.execute() on every pre-mounted widget on
// the harness page (one CDP round-trip per widget for the fire+read), then
// polls each widget's data-hcaptcha-response attribute until all n slots
// return a fresh token. Implemented inline so a single sticky-tab borrow
// yields n tokens — this is the actual speedup over Playground mode where
// one widget can only hold one response at a time.
func (b *Browser) extractBatchHarness(ctx context.Context, n int) ([]string, error) {
	runCtx, cancel := context.WithTimeout(b.browser, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	var prevJSON string
	if err := chromedp.Run(runCtx, chromedp.Evaluate(readBatchResponsesJS(n), &prevJSON)); err != nil {
		return nil, fmt.Errorf("chromedp read prev batch: %w", err)
	}
	prev, err := parseStringArray(prevJSON, n)
	if err != nil {
		return nil, fmt.Errorf("chromedp read prev batch: %w", err)
	}

	if err := chromedp.Run(runCtx, chromedp.Evaluate(fireBatchExecuteJS(n), nil)); err != nil {
		return nil, fmt.Errorf("chromedp fire batch: %w", err)
	}

	var tokensJSON string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := chromedp.Run(runCtx, chromedp.Evaluate(readBatchResponsesJS(n), &tokensJSON)); err != nil {
			return nil, fmt.Errorf("chromedp read batch: %w", err)
		}
		tokens, perr := parseStringArray(tokensJSON, n)
		if perr != nil {
			return nil, perr
		}
		if allFresh(tokens, prev) {
			return tokens, nil
		}
		if err := chromedp.Sleep(50 * time.Millisecond).Do(runCtx); err != nil {
			return nil, fmt.Errorf("chromedp sleep: %w", err)
		}
	}
	return nil, fmt.Errorf("captcha batch timed out after 30s")
}

func fireBatchExecuteJS(n int) string {
	return fmt.Sprintf(`(() => {
		if (typeof hcaptcha === 'undefined') return '';
		const widgets = document.querySelectorAll('[data-hcaptcha-widget-id]');
		const N = Math.min(widgets.length, %d);
		for (let i = 0; i < N; i++) {
			const id = widgets[i].getAttribute('data-hcaptcha-widget-id');
			try { hcaptcha.execute(id, { async: true }); } catch (e) {}
		}
		return '';
	})()`, n)
}

func readBatchResponsesJS(n int) string {
	return fmt.Sprintf(`(() => {
		const widgets = document.querySelectorAll('[data-hcaptcha-widget-id]');
		const N = Math.min(widgets.length, %d);
		const out = [];
		for (let i = 0; i < N; i++) {
			out.push(widgets[i].getAttribute('data-hcaptcha-response') || '');
		}
		while (out.length < %d) out.push('');
		return JSON.stringify(out);
	})()`, n, n)
}

func allFresh(tokens, prev []string) bool {
	if len(tokens) != len(prev) {
		return false
	}
	for i := range tokens {
		if tokens[i] == "" || tokens[i] == prev[i] {
			return false
		}
	}
	return true
}

// HarnessPageFor returns an HTML harness page with N hidden hCaptcha widgets
// pre-mounted for sitekey. Served via data:text/html;base64 so Chrome does no
// network round-trip to build.nvidia.com — only to hcaptcha.com for the
// widget script. RAM drops from ~150–350MB per Chrome to ~50MB.
func HarnessPageFor(sitekey string, batch int) string {
	html := fmt.Sprintf(`<!doctype html><html><head>
<meta charset="utf-8"><title>captcha harness</title>
<style>body{margin:0;font-family:system-ui}#root{padding:8px</style>
</head><body>
<div id="root</div>
<script>
window.__SITEKEY = %q;
window.__BATCH = %d;
(function() {
  var s = document.createElement('script');
  s.src = 'https://hcaptcha.com/1/api.js?recaptchacompat=off';
  s.async = true;
  s.defer = true;
  document.head.appendChild(s);
  function mount() {
    var root = document.getElementById('root');
    for (var i = 0; i < window.__BATCH; i++) {
      var d = document.createElement('div');
      d.className = 'h-captcha';
      d.setAttribute('data-sitekey', window.__SITEKEY);
      d.id = 'hcaptcha-widget-' + i;
      root.appendChild(d);
    }
    if (typeof hcaptcha !== 'undefined') {
      var widgets = document.querySelectorAll('.h-captcha');
      for (var j = 0; j < widgets.length; j++) {
        try { hcaptcha.render(widgets[j], { sitekey: window.__SITEKEY }); } catch (e) {}
      }
    }
  }
  if (document.readyState === 'complete' || document.readyState === 'interactive') {
    mount();
  } else {
    document.addEventListener('DOMContentLoaded', mount);
  }
})();
</script>
</body</html>`, sitekey, batch)
	return "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(html))
}
