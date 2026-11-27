// Native integration check of the language server users install from the ZIP.
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {mkdtempSync,mkdirSync,writeFileSync,rmSync,readFileSync,readdirSync,statSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {tmpdir} from 'node:os';
import {join,resolve} from 'node:path';
import {pathToFileURL} from 'node:url';
import {Client} from './lsp-client.mjs';
assert.equal(process.platform,'linux');
const archive=resolve(process.argv[2]);const stage=mkdtempSync(join(tmpdir(),'lithe-lite-linux-'));let server,backend;
const inventory=root=>{const entries=[];const walk=path=>{const stat=statSync(path);entries.push([path,stat.mode,stat.isFile()?createHash('sha256').update(readFileSync(path)).digest('hex'):null]);if(stat.isDirectory())for(const name of readdirSync(path).sort())walk(join(path,name));};walk(root);return entries;};
try{
 execFileSync('unzip',['-q',archive,'-d',stage],{timeout:15000});
 const pkg=join(stage,'io.github.lithe-idea.java-navigation');const before=inventory(pkg);
 const workspace=join(stage,'workspace');mkdirSync(workspace);const file=join(workspace,'Hello.java');writeFileSync(file,'class Hello { String value; }\n');
 const manifest=JSON.parse(readFileSync(join(pkg,'plugin.json'),'utf8'));
 backend=new Client(join(pkg,manifest.backend.launch.path),manifest.backend.arguments);
 await backend.request('initialize',{rootUri:pathToFileURL(workspace).href,capabilities:{experimental:{litheFunctions:{versions:[0]},litheLogging:{versions:[0]}}}});
 backend.notify('initialized',{});
 const state=join(stage,'state');
 const launch=await backend.request('lithe/function/invoke',{
  operationID:'package-smoke',functionID:manifest.languageServers[0].launch.function,
  context:{contextID:'workspace',generation:1},
  params:{serverID:'java',stateDirectory:state,platform:{os:process.platform}},
 });
 assert.equal(launch.executable.kind,'packaged');
 assert.deepEqual(launch.arguments,['--state-dir',state]);
 server=new Client(join(pkg,launch.executable.path),launch.arguments);
 const initialized=await server.request('initialize',{processId:null,rootUri:pathToFileURL(workspace).href,capabilities:{textDocument:{documentSymbol:{hierarchicalDocumentSymbolSupport:true}}}});
 assert.equal(initialized.capabilities.definitionProvider,true);server.notify('initialized',{});
 server.notify('textDocument/didOpen',{textDocument:{uri:pathToFileURL(file).href,languageId:'java',version:1,text:readFileSync(file,'utf8')}});
 const symbols=await server.request('textDocument/documentSymbol',{textDocument:{uri:pathToFileURL(file).href}});
 assert.ok(symbols.some(symbol=>symbol.name==='Hello'));
 await server.request('shutdown',null);server.notify('exit',null);await server.close();server=undefined;
 assert.deepEqual(inventory(pkg),before);
 console.log(`Verified Linux ${process.arch}: backend launch without a Java tool, LSP initialize, document symbols, shutdown and immutable package`);
}finally{try{await Promise.all([server?.close(),backend?.close()]);}finally{rmSync(stage,{recursive:true,force:true});}}
