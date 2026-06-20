(() => {
  const out = {};
  const cv = document.querySelector('#lyric-effect-canvas-id');
  out.canvasFound = !!cv;
  if (cv) {
    out.canvas = { w: cv.width, h: cv.height, css: cv.style.cssText, opacity: getComputedStyle(cv).opacity };
    // context type detection (non-destructive: getContext returns the existing one)
    let ctxType = 'unknown';
    try { if (cv.getContext('2d', {willReadFrequently:false})) ctxType = '2d'; } catch(e) {}
    try { if (cv.getContext('webgl2') || cv.getContext('webgl')) ctxType = 'webgl'; } catch(e) {}
    out.ctxTypeGuess = ctxType;
    // toDataURL feasibility
    try {
      const d = cv.toDataURL('image/png');
      out.toDataURL = { ok: true, len: d.length, blankish: d.length < 5000 };
    } catch(e) { out.toDataURL = { ok: false, err: e.message }; }
    // parent chain
    let chain = [], p = cv;
    for (let i=0;i<6 && p;i++){ chain.push(p.tagName + (p.id?('#'+p.id):'') + (p.className?('.'+String(p.className).split(' ').join('.')):'')); p = p.parentElement; }
    out.parentChain = chain;
  }
  // LyricEffectPageContainer children
  const cont = document.querySelector('[class^="LyricEffectPageContainer"]');
  out.containerFound = !!cont;
  if (cont) {
    out.containerBg = getComputedStyle(cont).backgroundColor;
    out.containerChildren = [...cont.children].map(c => ({
      tag: c.tagName, id: c.id, cls: c.className,
      w: c.width||c.offsetWidth, h: c.height||c.offsetHeight,
      bg: getComputedStyle(c).backgroundColor,
      bgImg: getComputedStyle(c).backgroundImage.slice(0,60),
      pos: getComputedStyle(c).position, z: getComputedStyle(c).zIndex,
      opacity: getComputedStyle(c).opacity
    }));
  }
  // canvasRaw
  const raw = document.querySelector('#canvasRaw');
  if (raw) out.canvasRaw = { w: raw.width, h: raw.height, css: raw.style.cssText };
  // effect-mask
  const mask = document.querySelector('[class^="effect-mask"]');
  if (mask) out.effectMask = { cls: mask.className, css: mask.style.cssText, bg: getComputedStyle(mask).background.slice(0,120) };
  // visibility
  out.vis = { state: document.visibilityState, hidden: document.hidden, hasFocus: document.hasFocus() };
  // is the song detail page open?
  out.songPageOpen = !!document.querySelector('#page_pc_songplay');
  return JSON.stringify(out, null, 2);
})()
