(function(){
  var prev=pvNav.prev, next=pvNav.next, back=pvNav.back;
  var infoPanel = document.getElementById('infoPanel');
  var infoBtn   = document.getElementById('infoBtn');
  var favBtn    = document.getElementById('favBtn');
  var vmedia    = document.getElementById('vmedia');

  function go(href){ if (href) location.href = href; }

  function toggleInfo() {
    if (!infoPanel) return;
    if (infoPanel.hasAttribute('hidden')) {
      infoPanel.removeAttribute('hidden');
      if (!infoPanel.dataset.loaded) {
        loadInfo();
      }
    } else {
      infoPanel.setAttribute('hidden', '');
    }
  }

  function row(label, value) {
    var l = document.createElement('div');
    l.className = 'ip-label';
    l.textContent = label;
    var v = document.createElement('div');
    v.className = 'ip-value';
    v.textContent = value;
    infoPanel.appendChild(l);
    infoPanel.appendChild(v);
  }

  function loadInfo() {
    if (!infoBtn) return;
    var id = infoBtn.dataset.id;
    if (!id) return;
    infoPanel.textContent = 'Loading...';
    fetch('/api/info?id=' + encodeURIComponent(id), { credentials: 'same-origin' })
      .then(function(r){ return r.json(); })
      .then(function(d){
        infoPanel.textContent = '';
        var title = document.createElement('h3');
        title.className = 'ip-title';
        title.textContent = d.name || '';
        infoPanel.appendChild(title);
        row('Path', d.path);
        row('Type', d.type);
        row('Size', d.size);
        row('Created', d.created);
        row('Modified', d.modified);
        row('Dimensions', d.dimensions);
        row('Camera', d.camera);
        row('Lens', d.lens);
        var settings = [d.aperture, d.shutter_speed, 'ISO ' + d.iso, d.focal_length].join('  ');
        row('Settings', settings);
        row('Favorite', d.favorite ? 'Yes' : 'No');
        infoPanel.dataset.loaded = '1';
      })
      .catch(function(){ infoPanel.textContent = 'Failed to load info.'; });
  }

  function toggleFav() {
    if (!favBtn) return;
    var id = favBtn.dataset.id;
    if (!id) return;
    fetch('/api/favorite', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id, toggle: true }),
    }).then(function(r){ return r.json(); })
      .then(function(d){
        if (d.favorite) {
          favBtn.classList.add('on');
          favBtn.textContent = '★';
        } else {
          favBtn.classList.remove('on');
          favBtn.textContent = '☆';
        }
        // Invalidate any cached info panel so its Favorite row re-renders
        // on the next open.
        if (infoPanel) infoPanel.dataset.loaded = '';
      })
      .catch(function(){});
  }

  if (infoBtn) infoBtn.addEventListener('click', toggleInfo);
  if (favBtn)  favBtn.addEventListener('click', toggleFav);

  document.addEventListener('keydown', function(e){
    if (e.target && /^(input|textarea)$/i.test(e.target.tagName)) return;
    var key = e.key;
    if (key === 'ArrowLeft' || key === 'h' || key === 'k') { go(prev); return; }
    if (key === 'ArrowRight'|| key === 'l' || key === 'j') { go(next); return; }
    if (key === 'Escape' || key === 'q') { go(back); return; }
    if (key === 'f') { toggleFav(); e.preventDefault(); return; }
    if (key === 'i') { toggleInfo(); e.preventDefault(); return; }
    if (key === ' ' && vmedia && vmedia.tagName === 'VIDEO') {
      if (vmedia.paused) vmedia.play(); else vmedia.pause();
      e.preventDefault();
      return;
    }
    if (key === 'm' && vmedia && vmedia.tagName === 'VIDEO') {
      vmedia.muted = !vmedia.muted;
      e.preventDefault();
      return;
    }
    if ((key === '[' || key === ']') && vmedia && vmedia.tagName === 'VIDEO') {
      vmedia.currentTime += (key === ']' ? 5 : -5);
      e.preventDefault();
      return;
    }
  });
})();
