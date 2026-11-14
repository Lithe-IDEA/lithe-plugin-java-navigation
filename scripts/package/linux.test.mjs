// Integration checks of the release archives, after scripts/package.mjs.
import test from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {readFileSync,mkdtempSync,rmSync,statSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join,resolve} from 'node:path';
import {createHash} from 'node:crypto';
const id="io.github.lithe-idea.java-navigation";
const version=JSON.parse(readFileSync(`${id}/plugin.json`)).version;
const read=(archive,path)=>execFileSync('unzip',['-p',archive,`${id}/${path}`],{timeout:10000,maxBuffer:16*1024*1024});
for (const [arch,machine] of [['arm64',183],['x64',62]]) {
 test(`Linux ${arch} archive contains an executable ELF language server`,t=>{
  const archive=resolve(`dist/java-navigation-${version}-linux-${arch}.zip`);
  const manifest=JSON.parse(read(archive,'plugin.json'));
  assert.equal(manifest.version,version);
  const path=manifest.languageServers[0].launch.path;
  assert.equal(path,'bin/java-navigation');
  const binary=read(archive,path);
  assert.deepEqual([...binary.subarray(0,6)],[127,69,76,70,2,1]);
  assert.equal(binary.readUInt16LE(18),machine);
  const stage=mkdtempSync(join(tmpdir(),'lithe-linux-archive-'));
  t.after(()=>rmSync(stage,{recursive:true,force:true}));
  execFileSync('unzip',['-q',archive,'-d',stage],{timeout:10000});
  assert.ok(statSync(join(stage,id,path)).mode & 0o111);
 });
}
