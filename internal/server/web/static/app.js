// The Contacts tab, inside muster's frame. The session token lives in this
// script's memory only: it is read from the page once, the attribute is
// removed, and it is never written to storage or a cookie. A reload finds no
// token and the server says "Open Contacts from muster again".
//
// The list (search on the server as you type), one contact's sheet (emails,
// note, the meetings you met in, where they came from), Add and Edit in the
// same sheet, Delete behind a confirmation, and Settings. Every name, email,
// title and note is rendered as text only (textContent).
(function () {
  'use strict';
  var root = document.getElementById('app');
  if (!root) return;
  var token = root.getAttribute('data-session');
  var ref = root.getAttribute('data-ref') || '';
  var meetingsBase = root.getAttribute('data-meetings') || '';
  root.removeAttribute('data-session');
  root.removeAttribute('data-ref');
  // Take muster's one-time code out of the address bar and the history.
  try { history.replaceState(null, '', location.pathname); } catch (e) { /* not fatal */ }

  var UI = window.ContactsUI, K = window.MusterKit;
  var $ = function (id) { return document.getElementById(id); };
  var list = $('contacts'), empty = $('empty'), more = $('more'), status = $('status'), q = $('q');
  var sheet = $('sheet'), form = $('edit');
  var next = null, seq = 0;
  var current = null; // the contact in the sheet
  var mode = 'view';  // view, edit or add
  var merging = null; // {id, name}: the contact holding an email the edit adds

  UI.decorate(document);

  function say(text) { status.textContent = text || ''; }

  function ended() {
    token = null;
    document.body.textContent = '';
    var m = document.createElement('main');
    m.className = 'page notice';
    var h = document.createElement('h1');
    h.textContent = 'Contacts';
    var p = document.createElement('p');
    p.textContent = 'Your session ended. Open Contacts from muster again.';
    m.appendChild(h); m.appendChild(p);
    document.body.appendChild(m);
  }

  function request(method, path, body) {
    if (!token) return Promise.reject(new Error('no session'));
    var init = { method: method, headers: { 'Authorization': 'Bearer ' + token }, cache: 'no-store', credentials: 'omit' };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    return fetch(path, init).then(function (res) {
      if (res.status === 401) { ended(); throw new Error('session ended'); }
      return res;
    });
  }

  function api(method, path, body) {
    return request(method, path, body).then(function (res) {
      if (res.status === 204) return {};
      return res.json().catch(function () { return {}; }).then(function (b) {
        if (!res.ok) { var e = new Error(b.error || ('the server answered ' + res.status)); e.status = res.status; e.body = b; throw e; }
        return b;
      });
    });
  }

  function day(iso) {
    if (!iso) return '';
    return new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' });
  }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  }

  function chip(iconName, text, kind) {
    var c = el('span', 'mu-chip' + (kind ? ' mu-chip--' + kind : ''));
    if (iconName) c.appendChild(UI.icon(iconName));
    c.appendChild(document.createTextNode(text));
    return c;
  }

  function avatar(node, name) {
    node.textContent = UI.initials(name);
  }

  // ---- the list ----

  function row(c) {
    var li = el('li', 'contact');
    li.setAttribute('data-id', c.id);
    var b = el('button', 'contact__main');
    b.type = 'button';
    var av = el('span', 'mu-avatar mu-avatar--sm');
    av.setAttribute('aria-hidden', 'true');
    avatar(av, c.name || c.email);
    var text = el('span', 'contact__text');
    text.appendChild(el('span', 'contact__name', c.name || c.email));
    if (c.name && c.email) text.appendChild(el('span', 'contact__email muted', c.email));
    b.appendChild(av);
    b.appendChild(text);
    if (c.origin === 'self_submitted') {
      var x = chip('send', 'Added by them');
      x.classList.add('origin');
      if (c.origin_title) x.setAttribute('title', c.origin_title);
      b.appendChild(x);
    }
    b.addEventListener('click', function () { openContact(c.id); });
    li.appendChild(b);
    return li;
  }

  function loadList(reset) {
    var mine = ++seq;
    var path = '/api/contacts?q=' + encodeURIComponent(q.value.trim());
    if (!reset && next) path += '&cursor=' + encodeURIComponent(next);
    return api('GET', path).then(function (b) {
      if (mine !== seq) return; // a newer search answered first
      if (reset) list.textContent = '';
      b.contacts.forEach(function (c) { list.appendChild(row(c)); });
      next = b.next;
      more.hidden = !next;
      var none = list.children.length === 0;
      empty.hidden = !none;
      $('empty-text').textContent = q.value.trim() ? 'No one matches.' : 'People you meet in Meetings appear here.';
      say('');
    }).catch(function (e) { if (token) say('Could not load contacts: ' + e.message); });
  }

  var debounce = null;
  q.addEventListener('input', function () {
    clearTimeout(debounce);
    debounce = setTimeout(function () { loadList(true); }, 200);
  });
  more.addEventListener('click', function () { loadList(false); });

  // ---- one contact ----

  function originLine(p) {
    var li = el('li', 'line origin-line');
    var icons = { owner_entered: 'mc-person', self_submitted: 'send', merged: 'send' };
    li.appendChild(UI.icon(p.meeting_id || p.kind !== 'merged' ? icons[p.kind] : 'mc-person'));
    var t = p.meeting_title || 'a meeting';
    var text;
    switch (p.kind) {
      case 'owner_entered': text = 'Added by you'; break;
      case 'self_submitted': text = 'Added by them after ' + t; break;
      default: // merged
        text = p.meeting_id ? 'Also sent their details after ' + t : 'Merged with ' + (p.said_name || 'another contact');
        if (p.meeting_id && p.said_name) text += ' · they said “' + p.said_name + '”';
    }
    li.appendChild(el('span', 'line__text', text));
    li.setAttribute('data-kind', p.kind);
    return li;
  }

  function meetingLine(m) {
    var li = el('li', 'line meeting-line');
    var body = el(meetingsBase ? 'a' : 'span', 'line__main');
    if (meetingsBase) {
      // The frame may open a popup, not navigate muster's own window.
      body.href = meetingsBase + '?ref=' + encodeURIComponent(m.meeting_id);
      body.target = '_blank';
      body.rel = 'noopener';
    }
    body.appendChild(el('span', 'line__title', m.title || 'Meeting'));
    body.appendChild(el('span', 'line__meta muted', day(m.started_at || m.at)));
    li.appendChild(body);
    if (m.unconfirmed) li.appendChild(chip('warning', 'email not confirmed', 'warning'));
    return li;
  }

  function copyText(text) {
    var done = function () { UI.toast('Copied.', { kind: 'success', timeout: 2000 }); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(done, function () { UI.toast('Press Ctrl+C (or ⌘C) to copy.'); });
    }
    UI.toast('Press Ctrl+C (or ⌘C) to copy.');
  }

  function renderView(c) {
    var title = c.name || (c.emails[0] && c.emails[0].address) || '';
    $('sheet-title').textContent = title;
    avatar($('sheet-avatar'), title);
    var emails = $('d-emails');
    emails.textContent = '';
    c.emails.forEach(function (e) {
      var li = el('li', 'line email-line');
      li.appendChild(UI.icon('mc-envelope'));
      li.appendChild(el('span', 'line__text address', e.address));
      if (e.source === 'self' && !e.confirmed) li.appendChild(chip('', 'not confirmed', 'warning'));
      li.appendChild(UI.iconButton('copy', 'Copy ' + e.address, function () { copyText(e.address); }));
      emails.appendChild(li);
    });
    var note = $('d-note');
    note.textContent = c.note || '';
    note.hidden = !c.note;
    var ms = $('d-meetings');
    ms.textContent = '';
    c.meetings.forEach(function (m) { ms.appendChild(meetingLine(m)); });
    $('d-met').hidden = c.meetings.length === 0;
    var og = $('d-origin');
    og.textContent = '';
    c.provenance.forEach(function (p) { og.appendChild(originLine(p)); });
  }

  function setMode(m) {
    mode = m;
    var editing = m !== 'view';
    $('view').hidden = editing;
    form.hidden = !editing;
    $('edit-footer').hidden = !editing;
    ['btn-edit', 'btn-download', 'btn-delete'].forEach(function (id) { $(id).hidden = editing; });
    $('f-error').hidden = true;
    $('f-merge').hidden = true;
    merging = null;
    if (m === 'add') { $('sheet-title').textContent = 'New contact'; avatar($('sheet-avatar'), ''); }
    if (m === 'view' && current) renderView(current);
  }

  function emailField(value) {
    var wrap = el('div', 'email-field');
    var input = el('input', 'mu-input');
    input.type = 'email';
    input.autocomplete = 'off';
    input.spellcheck = false;
    input.maxLength = 254;
    input.value = value || '';
    input.setAttribute('aria-label', 'Email');
    wrap.appendChild(input);
    wrap.appendChild(UI.iconButton('minus', 'Remove email', function () { wrap.remove(); }));
    return wrap;
  }

  function fillForm(c) {
    $('f-name').value = c ? c.name : '';
    $('f-note').value = c ? c.note : '';
    var box = $('f-emails');
    box.textContent = '';
    var addrs = c ? c.emails.map(function (e) { return e.address; }) : [];
    if (!addrs.length) addrs = [''];
    addrs.forEach(function (a) { box.appendChild(emailField(a)); });
  }

  $('f-add-email').addEventListener('click', function () {
    var f = emailField('');
    $('f-emails').appendChild(f);
    f.querySelector('input').focus();
  });

  function openSheet() {
    if (!sheet._muOpen) K.openSheet(sheet, { onClose: function () { current = null; setMode('view'); } });
  }

  function openContact(id) {
    return api('GET', '/api/contacts/' + encodeURIComponent(id)).then(function (b) {
      current = b.contact;
      setMode('view');
      openSheet();
    }).catch(function (e) { if (token) say('Could not open the contact: ' + e.message); });
  }

  $('btn-add').addEventListener('click', function () {
    current = null;
    fillForm(null);
    setMode('add');
    openSheet();
    $('f-name').focus();
  });

  $('btn-edit').addEventListener('click', function () {
    fillForm(current);
    setMode('edit');
    $('f-name').focus();
  });

  $('btn-cancel').addEventListener('click', cancelEdit);
  function cancelEdit() {
    if (mode === 'add') { K.closeSheet(sheet); return; }
    setMode('view');
  }
  // Escape leaves the form (the kit would close the whole sheet).
  sheet.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && mode !== 'view') { e.preventDefault(); e.stopPropagation(); cancelEdit(); }
  });

  function formBody() {
    var emails = Array.prototype.map.call($('f-emails').querySelectorAll('input'), function (i) { return i.value; })
      .filter(function (v) { return v.trim() !== ''; });
    return { name: $('f-name').value, emails: emails, note: $('f-note').value };
  }

  function showError(e) {
    var b = e.body || {};
    var err = $('f-error');
    Array.prototype.forEach.call(form.querySelectorAll('[aria-invalid]'), function (n) { n.removeAttribute('aria-invalid'); });
    if (e.status === 409 && b.conflict) {
      merging = b.conflict;
      $('f-merge-text').textContent = (b.conflict.name || b.conflict.address) + ' already has this email.';
      $('f-merge-btn').textContent = mode === 'edit' ? 'Merge' : 'Open';
      $('f-merge').hidden = false;
      err.hidden = true;
      return;
    }
    var msg = 'Could not save: ' + e.message;
    if (b.field === 'emails') {
      var inputs = $('f-emails').querySelectorAll('input');
      var filled = Array.prototype.filter.call(inputs, function (i) { return i.value.trim() !== ''; });
      var bad = filled[b.index] || inputs[0];
      if (bad) bad.setAttribute('aria-invalid', 'true');
      msg = 'Check the email address.';
    } else if (b.field === 'name') {
      $('f-name').setAttribute('aria-invalid', 'true');
      msg = 'Type a name or an email.';
    } else if (b.field === 'note') {
      $('f-note').setAttribute('aria-invalid', 'true');
      msg = 'The note is too long.';
    }
    err.textContent = msg;
    err.hidden = false;
  }

  function saved(c, created) {
    current = c;
    setMode('view');
    loadList(true).then(function () {
      var li = list.querySelector('li[data-id="' + c.id + '"]');
      if (li && created) li.classList.add('new');
    });
    UI.toast(created ? 'Added.' : 'Saved.', { kind: 'success', timeout: 2000 });
  }

  form.addEventListener('submit', function (e) {
    e.preventDefault();
    var body = formBody();
    if (mode === 'add') {
      api('POST', '/api/contacts', body).then(function (b) { saved(b.contact, true); }, showError);
    } else if (current) {
      api('PATCH', '/api/contacts/' + encodeURIComponent(current.id), body).then(function (b) { saved(b.contact, false); }, showError);
    }
  });

  $('f-merge-btn').addEventListener('click', function () {
    if (!merging) return;
    var other = merging;
    if (mode !== 'edit' || !current) { openContact(other.id); return; }
    // Merge the other contact into this one, then save the edit.
    api('POST', '/api/contacts/' + encodeURIComponent(current.id) + '/merge', { from: other.id }).then(function () {
      return api('PATCH', '/api/contacts/' + encodeURIComponent(current.id), formBody());
    }).then(function (b) { saved(b.contact, false); }, showError);
  });

  // ---- download ----

  function save(path, name) {
    return request('GET', path).then(function (res) {
      if (!res.ok) throw new Error('the server answered ' + res.status);
      return res.blob();
    }).then(function (blob) {
      var a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = name;
      document.body.appendChild(a);
      a.click();
      setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
    }).catch(function (e) { if (token) UI.toast('Could not download: ' + e.message, { kind: 'error' }); });
  }

  $('btn-download').addEventListener('click', function () {
    if (current) save('/api/contacts/' + encodeURIComponent(current.id) + '/export', 'contact.json');
  });

  // ---- delete ----

  var confirmSheet = $('confirm');
  $('btn-delete').addEventListener('click', function () {
    if (!current) return;
    var c = current;
    $('confirm-title').textContent = 'Delete ' + ($('sheet-title').textContent || 'this contact') + '?';
    K.closeSheet(sheet);
    current = c;
    K.openSheet(confirmSheet, { onClose: function () { current = null; } });
  });

  $('confirm-delete').addEventListener('click', function () {
    var c = current;
    if (!c) return;
    api('DELETE', '/api/contacts/' + encodeURIComponent(c.id)).then(function () {
      K.closeSheet(confirmSheet);
      var li = list.querySelector('li[data-id="' + c.id + '"]');
      if (li) UI.remove(li);
      setTimeout(function () { empty.hidden = list.querySelectorAll('li:not(.leaving)').length > 0; }, UI.dur('base'));
      UI.toast('Deleted.', { timeout: 2000 });
    }).catch(function (e) { UI.toast('Could not delete: ' + e.message, { kind: 'error' }); });
  });

  // ---- settings ----

  var settings = $('settings'), collect = $('s-collect'), confirmBox = $('s-confirm'), erase = $('s-erase');
  $('btn-settings').addEventListener('click', function () {
    api('GET', '/api/settings').then(function (b) {
      collect.checked = !!b.collect;
      confirmBox.value = '';
      erase.disabled = true;
      K.openSheet(settings);
    }).catch(function (e) { if (token) say('Could not open settings: ' + e.message); });
  });
  collect.addEventListener('change', function () {
    var want = collect.checked;
    api('PUT', '/api/settings', { collect: want }).catch(function (e) {
      collect.checked = !want;
      UI.toast('Could not save: ' + e.message, { kind: 'error' });
    });
  });
  $('s-download').addEventListener('click', function () { save('/api/export', 'contacts.json'); });
  confirmBox.addEventListener('input', function () { erase.disabled = confirmBox.value.trim().toLowerCase() !== 'delete everything'; });
  erase.addEventListener('click', function () {
    api('DELETE', '/api/all', { confirm: 'delete everything' }).then(function () {
      K.closeSheet(settings);
      q.value = '';
      loadList(true);
      UI.toast('Every contact was deleted.', { timeout: 3000 });
    }).catch(function (e) { UI.toast('Could not delete: ' + e.message, { kind: 'error' }); });
  });

  // ---- the session ----

  setInterval(function () { if (token) request('GET', '/api/alive').catch(function () {}); }, 30000);

  loadList(true).then(function () { if (ref) openContact(ref); });
})();
