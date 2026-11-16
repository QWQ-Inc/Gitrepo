'use strict';

// Light / dark theme. Loaded synchronously in <head> so data-theme is set
// before the first paint. The choice ("light", "dark", or "system" = follow
// the operating system) is kept in localStorage; the design kit only reads
// data-theme on <html>.
(function () {
  var KEY = 'gitrepo.theme';
  var mq = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  var current = 'system';
  try {
    var saved = localStorage.getItem(KEY);
    if (saved === 'light' || saved === 'dark') current = saved;
  } catch (_) { /* storage blocked: follow the system */ }

  function apply() {
    var dark = current === 'dark' || (current === 'system' && mq !== null && mq.matches);
    var root = document.documentElement;
    root.setAttribute('data-theme', dark ? 'dark' : 'light');
    root.setAttribute('data-theme-choice', current);
  }

  apply();
  if (mq && mq.addEventListener) mq.addEventListener('change', apply);

  window.gitrepoTheme = {
    get: function () { return current; },
    set: function (c) {
      current = c === 'light' || c === 'dark' ? c : 'system';
      try {
        if (current === 'system') localStorage.removeItem(KEY);
        else localStorage.setItem(KEY, current);
      } catch (_) { /* storage blocked: the choice lasts for this page */ }
      apply();
    },
  };
})();
