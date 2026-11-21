(function(){
  var grid = document.getElementById('grid');
  if (!grid) return;
  var loader = document.getElementById('loader');
  var nextPage = parseInt(grid.dataset.nextPage || '2', 10);
  var hasNext  = grid.dataset.hasNext === 'true';
  var from     = grid.dataset.from || '';
  var loading  = false;
  var selected = -1;
  var gPending = false;

  function makeStar() {
    var star = document.createElement('span');
    star.className = 'star';
    star.title = 'Favorite';
    star.textContent = '★';
    return star;
  }

  function setSelected(all, i) {
    if (all.length === 0) return;
    if (i < 0) i = 0;
    if (i >= all.length) i = all.length - 1;
    if (selected >= 0 && selected < all.length) all[selected].classList.remove('focus');
    selected = i;
    all[i].classList.add('focus');
    all[i].scrollIntoView({block: 'nearest', inline: 'nearest'});
    all[i].focus({preventScroll: true});
  }

  function colsPerRow(all) {
    if (all.length < 2) return 1;
    var firstTop = all[0].getBoundingClientRect().top;
    for (var i = 1; i < all.length; i++) {
      if (all[i].getBoundingClientRect().top > firstTop + 1) return i;
    }
    return all.length;
  }

  function buildCell(item) {
    var a = document.createElement('a');
    a.className = 'cell';
    a.href = '/view/' + item.id + (from ? '?' + from : '');
    a.title = item.name;
    a.dataset.id = item.id;

    var img = document.createElement('img');
    img.loading = 'lazy';
    img.decoding = 'async';
    img.alt = '';
    img.src = '/thumb/' + item.id;
    a.appendChild(img);

    if (item.video) {
      var badge = document.createElement('span');
      badge.className = 'badge';
      badge.textContent = 'video';
      a.appendChild(badge);
    }
    if (item.favorite) {
      a.appendChild(makeStar());
    }

    var name = document.createElement('span');
    name.className = 'name';
    name.textContent = item.name;
    a.appendChild(name);
    return a;
  }

  function fetchNext() {
    if (loading || !hasNext || !loader) return;
    loading = true;
    var url = '/api/page?' + from + '&p=' + nextPage;
    var last = grid.lastElementChild;
    if (last && last.dataset && last.dataset.id) url += '&after=' + encodeURIComponent(last.dataset.id);
    fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
      .then(function(r){ return r.json(); })
      .then(function(data){
        if (data && data.items) {
          var frag = document.createDocumentFragment();
          for (var i = 0; i < data.items.length; i++) {
            frag.appendChild(buildCell(data.items[i]));
          }
          grid.appendChild(frag);
        }
        hasNext = !!(data && data.hasNext);
        nextPage++;
        loading = false;
        if (!hasNext && loader) {
          loader.remove();
          obs.disconnect();
        }
      })
      .catch(function(){
        loading = false;
        if (loader) loader.textContent = 'Failed to load - scroll to retry.';
      });
  }

  var obs;
  if (loader) {
    obs = new IntersectionObserver(function(entries){
      entries.forEach(function(e){ if (e.isIntersecting) fetchNext(); });
    }, { rootMargin: '600px 0px' });
    obs.observe(loader);
  }

  document.addEventListener('keydown', function(e){
    if (e.target && /^(input|textarea|select)$/i.test(e.target.tagName)) return;
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    // One live HTMLCollection per keypress instead of three querySelectorAll
    // walks; the grid holds only cells.
    var all = grid.children;
    if (all.length === 0) return;
    var cols = colsPerRow(all);
    var key = e.key;
    if (key === 'j' || key === 'ArrowDown') { setSelected(all, (selected < 0 ? 0 : selected + cols)); e.preventDefault(); return; }
    if (key === 'k' || key === 'ArrowUp')   { setSelected(all, (selected < 0 ? 0 : selected - cols)); e.preventDefault(); return; }
    if (key === 'l' || key === 'ArrowRight'){ setSelected(all, (selected < 0 ? 0 : selected + 1));    e.preventDefault(); return; }
    if (key === 'h' || key === 'ArrowLeft') { setSelected(all, (selected < 0 ? 0 : selected - 1));    e.preventDefault(); return; }
    if (key === 'G') { setSelected(all, all.length - 1); e.preventDefault(); return; }
    if (key === 'g') {
      if (gPending) { setSelected(all, 0); gPending = false; e.preventDefault(); return; }
      gPending = true;
      setTimeout(function(){ gPending = false; }, 600);
      return;
    }
    if (key === 'Enter' && selected >= 0) { all[selected].click(); e.preventDefault(); return; }
    if (key === 'f' && selected >= 0) {
      toggleFavorite(all[selected]);
      e.preventDefault();
      return;
    }
  });

  function toggleFavorite(cell) {
    if (!cell) return;
    var id = cell.dataset.id;
    if (!id) return;
    fetch('/api/favorite', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: id }),
    }).then(function(r){ return r.json(); })
      .then(function(d){
        var star = cell.querySelector('.star');
        if (d.favorite) {
          if (!star) {
            star = makeStar();
            cell.appendChild(star);
          }
        } else if (star) {
          star.remove();
        }
      })
      .catch(function(){});
  }
})();
