'use strict';

const vm = require('vm');

const source = process.env.SIMPLE_HOST_PLATFORM_CLASSIFIER;
if (!source) throw new Error('missing classifier source');

const context = {};
vm.createContext(context);
vm.runInContext(source, context);

const cases = [
  {
    name: 'Windows navigator fallback',
    navigator: { platform: 'Win32' },
    marker: 'Browser hint: Windows.'
  },
  {
    name: 'macOS navigator fallback',
    navigator: { platform: 'MacIntel' },
    marker: 'Browser hint: macOS.'
  },
  {
    name: 'unknown preferred userAgentData',
    navigator: { userAgentData: { platform: 'Linux' }, platform: 'Win32' },
    marker: 'Browser platform is unknown.'
  }
];

for (const testCase of cases) {
  const hint = context.classifyInstallPlatform(testCase.navigator);
  if (!hint.startsWith(testCase.marker)) {
    throw new Error(`${testCase.name}: got ${JSON.stringify(hint)}`);
  }
  if (!hint.includes('actual operating system and shell')) {
    throw new Error(`${testCase.name}: hint does not require OS/shell verification`);
  }
}
