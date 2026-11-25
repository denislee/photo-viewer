(function(){
  var prev=pvNav.prev, next=pvNav.next, back=pvNav.back;
  // Prefetch the next photo's display rendition once this page's media has
  // loaded, so it doesn't compete with it. new Image() rather than
  // <link rel=prefetch>, which iOS Safari ignores (W4-04).
  function prefetchNext() { if (pvNav.nextImg) { new Image().src = pvNav.nextImg; } }
  if (document.readyState === 'complete') prefetchNext();
  else window.addEventListener('load', prefetchNext);
  var infoPanel = document.getElementById('infoPanel');
  var infoBtn   = document.getElementById('infoBtn');
  var favBtn    = document.getElementById('favBtn');
  var vmedia    = document.getElementById('vmedia');
  var faceBtn   = document.getElementById('faceBtn');
  var vstage    = document.querySelector('.vstage');
  var faces    = [];       // {box:[x,y,w,h], name, person} from /api/info
  var facesLoaded = false;
  var facesOn     = false;

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

  // Face overlay: /api/info's "faces" boxes are normalised to the rendition the
  // detector ran on, whose aspect ratio matches the image shown here, so each
  // box maps onto the rendered <img> rect. Boxes are plain divs over the stage
  // (pointer-events: none) labelled with the person's name.
  function clearFaces() {
    if (!vstage) return;
    var boxes = vstage.querySelectorAll('.facebox');
    for (var i = 0; i < boxes.length; i++) boxes[i].remove();
  }

  function paintFaces() {
    clearFaces();
    if (!facesOn || !vstage || !faces.length) return;
    var img = vstage.querySelector('img.vmedia');
    if (!img) return;
    var ir = img.getBoundingClientRect(), sr = vstage.getBoundingClientRect();
    for (var i = 0; i < faces.length; i++) {
      var box = faces[i].box;
      if (!box || box.length !== 4) continue;
      var d = document.createElement('div');
      d.className = 'facebox';
      d.style.left   = (ir.left - sr.left + box[0] * ir.width) + 'px';
      d.style.top    = (ir.top - sr.top + box[1] * ir.height) + 'px';
      d.style.width  = (box[2] * ir.width) + 'px';
      d.style.height = (box[3] * ir.height) + 'px';
      if (faces[i].name) {
        var l = document.createElement('span');
        l.className = 'facelabel';
        l.textContent = faces[i].name;
        d.appendChild(l);
      }
      vstage.appendChild(d);
    }
  }

  function setFaces(list) {
    faces = list || [];
    facesLoaded = true;
    if (faceBtn) faceBtn.hidden = faces.length === 0;
    paintFaces();
  }

  function loadFaces() {
    if (!faceBtn) return;
    var id = faceBtn.dataset.id;
    if (!id) return;
    fetch('/api/info?id=' + encodeURIComponent(id), { credentials: 'same-origin' })
      .then(function(r){ return r.json(); })
      .then(function(d){ setFaces(d.faces); })
      .catch(function(){ setFaces([]); });
  }

  function toggleFaces() {
    if (!vstage) return;
    facesOn = !facesOn;
    if (faceBtn) faceBtn.classList.toggle('on', facesOn);
    if (!facesOn) { clearFaces(); return; }
    if (facesLoaded) paintFaces(); else loadFaces();
  }

  function toggleFav() {
    if (!favBtn) return;
    var id = favBtn.dataset.id;
    if (!id) return;
    fetch('/api/favorite', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id }),
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
  if (faceBtn) faceBtn.addEventListener('click', toggleFaces);
  // The overlay is positioned from the rendered <img>, so it must be repainted
  // when that rect changes: after the image loads and on every resize.
  var mainImg = vstage ? vstage.querySelector('img.vmedia') : null;
  if (mainImg) mainImg.addEventListener('load', paintFaces);
  window.addEventListener('resize', paintFaces);

  document.addEventListener('keydown', function(e){
    if (e.target && /^(input|textarea)$/i.test(e.target.tagName)) return;
    // Leave browser shortcuts (Ctrl+L, Ctrl+F, Cmd+[ …) alone, as grid.js does.
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    var key = e.key;
    if (key === 'ArrowLeft' || key === 'h' || key === 'k') { go(prev); return; }
    if (key === 'ArrowRight'|| key === 'l' || key === 'j') { go(next); return; }
    if (key === 'Escape' || key === 'q') { go(back); return; }
    if (key === 'f') { toggleFav(); e.preventDefault(); return; }
    if (key === 'i') { toggleInfo(); e.preventDefault(); return; }
    if (key === 'p') { toggleFaces(); e.preventDefault(); return; }
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
