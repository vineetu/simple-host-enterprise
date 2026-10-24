const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
function mount(){
 const nodes=new Map(),events={},opened=[];
 const get=id=>{if(!nodes.has(id))nodes.set(id,{hidden:true,focus(){},select(){}});return nodes.get(id);};
 const app={on:(name,handler)=>events[name]=handler,start:async()=>{},openLink:async url=>opened.push(url)};
 const ctx=vm.createContext({window:{SHApp:app},document:{getElementById:get},navigator:{clipboard:{writeText:async()=>{throw Error('denied')}}},URL,setTimeout,clearTimeout});
 vm.runInContext(fs.readFileSync('internal/mcp/apps/site-preview.js','utf8'),ctx);
 return {get,events,opened};
}
test('preview card uses text, opens the returned URL, and never nests frames',async()=>{
 const m=mount();m.events['tool-result']({structuredContent:{name:'<img onerror=alert(1)>',url:'https://demo.alice.example.test/'}});
 assert.equal(m.get('preview-title').textContent,'<img onerror=alert(1)>');assert.equal(m.get('preview-full').hidden,true);
 await m.get('preview-open').onclick();assert.deepEqual(m.opened,['https://demo.alice.example.test/']);
 await m.get('preview-copy').onclick();assert.equal(m.get('preview-copy-fallback').hidden,false);assert.equal(m.get('preview-link').value,m.opened[0]);
});
test('unsafe preview URLs are not actionable',()=>{
 for(const url of ['javascript:alert(1)','https://user:password@example.test/']){
  const m=mount();m.events['tool-result']({structuredContent:{name:'demo',url}});
  assert.equal(m.get('preview-open').disabled,true);assert.equal(m.get('preview-copy').disabled,true);
 }
});
