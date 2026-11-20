/**
 * Frontend bridge for Antigravity Sidecar & Standalone agy-hq Web UI.
 */
(function() {
  const params = new URLSearchParams(window.location.search);
  const token = params.get('token') || '';
  const conversationId = params.get('conversationId') || '';

  window.addEventListener('message', (event) => {
    if (event.data && event.data.type === 'theme-change') {
      const vars = event.data.payload && event.data.payload.cssVariables;
      if (vars) {
        for (const [key, value] of Object.entries(vars)) {
          document.documentElement.style.setProperty(key, value);
        }
      }
    }
  });

  window.sidecar = {
    token: token,
    conversationId: conversationId,
    fetch: function(url, options) {
      options = options || {};
      options.headers = options.headers || {};
      if (token && !options.headers['x-sidecar-token']) {
        options.headers['x-sidecar-token'] = token;
      }
      return fetch(url, options);
    }
  };
})();