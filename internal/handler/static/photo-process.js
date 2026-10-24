/* Shared by /photos and the MCP photo picker. No network or account access. */
(function (root) {
  'use strict';
  const slug = name => name.toLowerCase().replace(/\.[^.]+$/, '').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 60) || 'photo';
  const dataURL = blob => new Promise((resolve, reject) => {
    const reader = new FileReader(); reader.onload = () => resolve(reader.result); reader.onerror = reject; reader.readAsDataURL(blob);
  });
  async function nativeDecode(file) {
    if (typeof createImageBitmap === 'function') {
      try {
        const bitmap = await createImageBitmap(file, { imageOrientation: 'from-image' });
        return { source: bitmap, width: bitmap.width, height: bitmap.height, orientation: 1, close: () => bitmap.close() };
      } catch (_) { /* Older browsers and HEIC may need another decoder. */ }
    }
    const url = await dataURL(file);
    const img = new Image();
    try {
      await new Promise((resolve, reject) => { img.onload = resolve; img.onerror = reject; img.src = url; });
      return { source: img, width: img.naturalWidth, height: img.naturalHeight, orientation: 1, close: () => { img.src = ''; } };
    } catch (_) { throw new Error('Native decode failed.'); }
  }
  async function decode(file, options = {}) {
    // Only File objects from the picker/drop handler enter decoding; never URLs.
    if (file.size > 80000000) throw new Error('Could not read this photo');
    const head = new Uint8Array(await file.slice(0, 256).arrayBuffer());
    const ascii = String.fromCharCode(...head);
    const heif = /\.(heic|heif)$/i.test(file.name) || /image\/(heic|heif)/i.test(file.type) || (ascii.slice(4, 8) === 'ftyp' && /heic|heix|hevc|heif|mif1|msf1/.test(ascii.slice(8)));
    const tiff = /\.tiff?$/i.test(file.name) || /tiff/i.test(file.type) || ascii.startsWith('II*\0') || ascii.startsWith('MM\0*');
    // These parsers always run off the UI thread, even on browsers with native support.
    if (!heif && !tiff) {
      const decoded = await nativeDecode(file);
      if (decoded.width * decoded.height > 50000000) { decoded.close(); throw new Error('Could not read this photo'); }
      return decoded;
    }
    if (options.nativeFirst) {
      try {
        const decoded = await nativeDecode(file);
        if (decoded.width * decoded.height <= 50000000) return decoded;
        decoded.close();
        throw new Error('Could not read this photo');
      } catch (_) { /* Use the same bounded codec worker as /photos. */ }
    }
    const buffer = await file.arrayBuffer();
    const worker = options.workerFactory ? options.workerFactory() : new Worker('/photo-codecs/v2/decoder.js');
    let timeout;
    try {
      const decoded = await new Promise((resolve, reject) => {
        timeout = setTimeout(() => { worker.terminate(); reject(new Error('Could not read this photo')); }, 20000);
        worker.onmessage = event => event.data.error ? reject(new Error(event.data.error)) : resolve(event.data);
        worker.onerror = () => reject(new Error('Could not read this photo'));
        worker.postMessage({ buffer, kind: heif ? 'heif' : 'tiff' }, [buffer]);
      });
      const canvas = document.createElement('canvas');
      canvas.width = decoded.width; canvas.height = decoded.height;
      canvas.getContext('2d').putImageData(new ImageData(new Uint8ClampedArray(decoded.buffer), decoded.width, decoded.height), 0, 0);
      return { source: canvas, width: canvas.width, height: canvas.height, orientation: decoded.orientation, close: () => { canvas.width = canvas.height = 1; } };
    } finally { clearTimeout(timeout); worker.terminate(); }
  }
  function draw(canvas, source, longSide) {
    const swapped = source.orientation >= 5 && source.orientation <= 8;
    const ratio = Math.min(1, longSide / Math.max(source.width, source.height));
    const w = Math.max(1, Math.round(source.width * ratio)), h = Math.max(1, Math.round(source.height * ratio));
    canvas.width = swapped ? h : w; canvas.height = swapped ? w : h;
    const ctx = canvas.getContext('2d');
    // TIFF orientation is separate; native decode and libheif apply theirs.
    const transforms = { 2: [-1,0,0,1,w,0], 3: [-1,0,0,-1,w,h], 4: [1,0,0,-1,0,h], 5: [0,1,1,0,0,0], 6: [0,1,-1,0,h,0], 7: [0,-1,-1,0,h,w], 8: [0,-1,1,0,0,w] };
    if (transforms[source.orientation]) ctx.setTransform(...transforms[source.orientation]);
    ctx.drawImage(source.source, 0, 0, w, h);
  }
  const blobFrom = (canvas, type, quality) => new Promise((resolve, reject) => canvas.toBlob(blob => blob ? resolve(blob) : reject(new Error('Could not make a small copy of this photo.')), type, quality));
  async function shrink(file, options = {}) {
    const byteCeiling = Math.min(950000, Math.floor((options.maxBytes || 1000000) * 0.95));
    const byteTarget = Math.min(300000, byteCeiling);
    const longSide = Math.min(1600, options.maxDimension || 2048);
    const decoded = await decode(file, options);
    const canvas = document.createElement('canvas');
    try { draw(canvas, decoded, longSide); } finally { decoded.close(); }
    try {
      let type = 'image/webp';
      let blob = await blobFrom(canvas, type, 0.8);
      if (blob.type !== type) { type = 'image/jpeg'; blob = await blobFrom(canvas, type, 0.8); }
      for (const quality of [0.7, 0.6, 0.5, 0.4]) {
        if (blob.size <= byteTarget) break;
        blob = await blobFrom(canvas, type, quality);
      }
      // Difficult noise-heavy images may need fewer pixels as well.
      while (blob.size > byteTarget && Math.max(canvas.width, canvas.height) > Math.min(640, longSide / 2)) {
        const smaller = document.createElement('canvas');
        smaller.width = Math.max(1, Math.round(canvas.width * 0.8));
        smaller.height = Math.max(1, Math.round(canvas.height * 0.8));
        smaller.getContext('2d').drawImage(canvas, 0, 0, smaller.width, smaller.height);
        canvas.width = smaller.width; canvas.height = smaller.height;
        canvas.getContext('2d').drawImage(smaller, 0, 0);
        smaller.width = smaller.height = 1;
        blob = await blobFrom(canvas, type, 0.7);
      }
      if (blob.type !== type || blob.size >= byteCeiling) throw new Error('Could not make this photo small enough. Try another copy.');
      return { blob, ext: type === 'image/webp' ? '.webp' : '.jpg' };
    } finally { canvas.width = canvas.height = 1; }
  }
  root.SHPhotos = { shrink, slug, dataURL };
})(globalThis);
