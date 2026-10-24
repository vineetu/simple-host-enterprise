/* One HEIC/TIFF decode per worker. Terminating it releases the Wasm heap too. */
const maxPixels = 50000000;
function checkDimensions(width, height) {
  if (!Number.isSafeInteger(width) || !Number.isSafeInteger(height) || width < 1 || height < 1 || width * height > maxPixels) throw new Error('Could not read this photo');
}
self.onmessage = async function (event) {
  try {
    const { buffer, kind } = event.data;
    if (!(buffer instanceof ArrayBuffer) || buffer.byteLength > 80000000) throw new Error('Could not read this photo');
    let pixels, width, height, orientation = 1;
    if (kind === 'heif') {
      const base = self.photoCodecBase || '';
      if (self.photoCodecBase) importScripts(base + 'wasm.js');
      importScripts(base + 'libheif.js');
      const heif = await libheif({ ...(self.photoWasm ? { wasmBinary: Uint8Array.from(atob(self.photoWasm), c => c.charCodeAt(0)) } : {}), stdin: () => null, locateFile: name => '/photo-codecs/v2/' + name });
      const decoder = new heif.HeifDecoder();
      const images = decoder.decode(new Uint8Array(buffer));
      if (!images.length) throw new Error('Could not read this HEIC photo.');
      const image = images.find(item => item.is_primary()) || images[0];
      width = image.get_width(); height = image.get_height();
      // decode() reads handles; display() below decodes pixels.
      checkDimensions(width, height);
      const data = { width, height, data: new Uint8ClampedArray(width * height * 4) };
      await new Promise((resolve, reject) => image.display(data, result => result ? resolve() : reject(new Error('Could not read this HEIC photo.'))));
      pixels = data.data;
      images.forEach(item => item.free());
    } else {
      const base = self.photoCodecBase || '';
      importScripts(base + 'pako.min.js', base + 'UTIF.js');
      const pages = UTIF.decode(buffer);
      const page = pages[0];
      if (!page) throw new Error('Could not read this TIFF photo.');
      checkDimensions((page.t256 || [])[0], (page.t257 || [])[0]);
      UTIF.decodeImage(buffer, page);
      width = page.width; height = page.height;
      orientation = (page.t274 || [1])[0];
      pixels = new Uint8ClampedArray(UTIF.toRGBA8(page));
    }
    if (!width || !height || pixels.length !== width * height * 4) throw new Error('Could not read this photo.');
    self.postMessage({ width, height, orientation, buffer: pixels.buffer }, [pixels.buffer]);
  } catch (error) {
    self.postMessage({ error: 'Could not read this photo' });
  }
};
