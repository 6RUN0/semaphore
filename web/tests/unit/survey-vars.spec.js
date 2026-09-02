import { expect } from 'chai';
import { shallowMount } from '@vue/test-utils';
import Vue from 'vue';
import Vuetify from 'vuetify';
import SurveyVars from '@/components/SurveyVars.vue';

Vue.use(Vuetify);

// Vuetify installs once per process, whichever constructor gets it first:
// the flag sits on its install function, not on the Vue it was given. In a
// full run an earlier spec installs it on its own local Vue, the install
// above is a no-op and nothing injects $vuetify, which the template reads.
// The mock covers that case; when the install did run, the injected one wins.
function mountVars(vars) {
  const vuetify = new Vuetify();
  return shallowMount(SurveyVars, {
    vuetify,
    propsData: { vars },
    mocks: { $t: (k) => k, $vuetify: vuetify.framework },
  });
}

describe('SurveyVars.vue', () => {
  // The server refuses a default on a secret variable, and the editor hides
  // the field for that type. A default stored before the rule existed would
  // otherwise be sent back untouched on every save, with no control to clear
  // it, so editing the variable has to drop it.
  it('drops the stored default of a secret variable on edit', () => {
    const wrapper = mountVars([{ name: 'pw', type: 'secret', default_value: 'hunter2' }]);

    wrapper.vm.editVar(0);

    expect(wrapper.vm.editedVar.default_value).to.equal('');
  });

  it('keeps the default of a non-secret variable on edit', () => {
    const wrapper = mountVars([{ name: 'host', type: '', default_value: 'db' }]);

    wrapper.vm.editVar(0);

    expect(wrapper.vm.editedVar.default_value).to.equal('db');
  });
});
