import { expect } from 'chai';
import fs from 'fs';
import path from 'path';

// This suite asserts on the component source rather than on a mounted DOM.
//
// The reason is the test setup, not preference: vue-loader in this webpack
// config does not hand the template back as a render function ("export 'render'
// was not found" during compilation), so a shallowMount here produces an empty
// wrapper for every component. The two pre-existing failures in
// example.spec.js and args-picker.spec.js are the same problem. Until that is
// fixed, a DOM assertion would fail for a reason unrelated to what it checks.
// process.cwd() rather than __dirname: webpack rewrites __dirname to a virtual
// path inside the bundle, which resolves to a directory that does not exist.
// The runner is started from web/, which is what `npm run test:unit` does.
const formSource = fs.readFileSync(
  path.resolve(process.cwd(), 'src/components/SecretStorageForm.vue'),
  'utf8',
);

describe('SecretStorageForm.vue', () => {
  // Skipping certificate verification makes Semaphore accept any certificate
  // from the Vault it holds a project token for. The flag lives in the stored
  // params and the form submits the whole item, so it survives any edit: with
  // no control for it, an active insecure setting is invisible in the UI and
  // cannot be turned back off there. It was dropped once already, while
  // resolving a merge conflict, which is what this pins.
  it('keeps the Vault TLS verification control', () => {
    expect(
      formSource,
      'the TLS skip-verify control must stay in the vault section',
    ).to.contain('data-testid="secretStorage-vaultTlsSkipVerify"');

    expect(
      formSource,
      'the control must be bound to the stored flag, not to a local field',
    ).to.contain('v-model="item.params.tls_skip_verify"');
  });

  // The vault section is shared with openbao (`v-if="item.type === 'vault' ||
  // item.type === 'openbao'"`), so the control must sit inside it rather than
  // in a vault-only branch that would leave openbao without it.
  it('places the control inside the section shared with openbao', () => {
    const sectionStart = formSource.indexOf("item.type === 'vault' || item.type === 'openbao'");
    const controlAt = formSource.indexOf('data-testid="secretStorage-vaultTlsSkipVerify"');

    expect(sectionStart, 'the shared vault/openbao section must exist').to.be.greaterThan(-1);
    expect(controlAt, 'the control must exist').to.be.greaterThan(-1);
    expect(
      controlAt,
      'the control must be rendered inside the shared vault/openbao section',
    ).to.be.greaterThan(sectionStart);
  });
});
