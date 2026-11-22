// The page's helpers over muster's UI kit (docs/ui-kit.md in ZacxDev/muster).
// kit.css and kit.js come from this plugin's own origin (/kit/v1/, served by
// internal/server kit.go: muster's live kit fetched server-side, or the kit
// bundled with the plugin when muster cannot be reached), so
// window.MusterKit is always there by the time this runs.
(function () {
  'use strict';
  var SVGNS = 'http://www.w3.org/2000/svg';
  var doc = document.documentElement;
  function K() { return window.MusterKit; }

  // icon is a decorative <svg class="mu-icon">: the kit's icon, or for a
  // name starting "mc-" one of this page's own (app.html's sprite).
  function icon(name) {
    if (name.indexOf('mc-') !== 0) return K().icon(name);
    var svg = document.createElementNS(SVGNS, 'svg');
    svg.setAttribute('class', 'mu-icon');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    var use = document.createElementNS(SVGNS, 'use');
    use.setAttribute('href', '#' + name);
    svg.appendChild(use);
    return svg;
  }

  // iconButton is a kit icon button: one icon, an aria-label (the kit draws
  // it as the tooltip).
  function iconButton(name, text, onClick) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'mu-icon-btn';
    b.appendChild(icon(name));
    b.setAttribute('data-icon', name);
    b.setAttribute('aria-label', text);
    if (onClick) b.addEventListener('click', onClick);
    return b;
  }

  // decorate gives every [data-icon] element in root its icon.
  function decorate(root) {
    Array.prototype.forEach.call((root || document).querySelectorAll('[data-icon]'), function (el) {
      if (!el.querySelector('svg.mu-icon')) el.insertBefore(icon(el.getAttribute('data-icon')), el.firstChild);
    });
  }

  function toast(message, opts) { return K().toast(message, opts); }

  // dur is a --mu-dur-<name> token in milliseconds: 0 under
  // prefers-reduced-motion (kit.css sets them to 0ms), so every transition
  // here stops with the kit's own.
  function dur(name) {
    var v = getComputedStyle(doc).getPropertyValue('--mu-dur-' + name).trim();
    var n = parseFloat(v);
    if (!isFinite(n)) return 0;
    return /ms$/.test(v) ? n : n * 1000;
  }

  // remove takes el out with its .leaving transition, then from the page;
  // at once under reduced motion.
  function remove(el) {
    var d = dur('base');
    if (!d) { el.remove(); return; }
    el.classList.add('leaving');
    el.setAttribute('aria-hidden', 'true');
    setTimeout(function () { el.remove(); }, d);
  }

  // initials of a name, for an avatar: the first letters of its first and
  // last words. Text only.
  function initials(name) {
    var parts = String(name || '').trim().split(/\s+/).filter(Boolean);
    if (!parts.length) return '?';
    var first = Array.from(parts[0])[0], last = parts.length > 1 ? Array.from(parts[parts.length - 1])[0] : '';
    return (first + last).toUpperCase();
  }

  window.ContactsUI = { icon: icon, iconButton: iconButton, decorate: decorate, toast: toast, dur: dur, remove: remove, initials: initials };
})();
