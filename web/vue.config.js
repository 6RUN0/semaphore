const webpack = require('webpack');

const mentionsLoader = (rule, name) => JSON.stringify(rule.use || rule.loader || '').includes(name);

/**
 * Runs vuetify-loader before babel-loader on the compiled render functions.
 *
 * vuetify-loader appends `import { VBtn } from 'vuetify/lib/...'` to the render
 * function of every template that uses a Vuetify component, and it runs last in
 * the chain. Under `test:unit` babel has already rewritten that render function
 * to CommonJS by then: @vue/babel-preset-app switches modules to commonjs
 * whenever NODE_ENV is 'test', a setting meant for Jest, which runs unbundled,
 * unlike mochapack. The module then carries ESM imports and CommonJS exports at
 * once, webpack treats it as ESM, `exports.render = ...` is dropped, and every
 * component is left without a render function -- so nothing renders and no DOM
 * assertion can hold.
 *
 * Swapping the two keeps the module consistently CommonJS. Babel only ever sees
 * the render function inside .vue files because vue-loader clones the js rule
 * for it, which it does for Vue 2.7 and up.
 *
 * The rule is created by VuetifyLoaderPlugin while webpack starts, which is why
 * it is moved here instead of through chainWebpack, and after every plugin has
 * been applied so that no later one can undo the order.
 */
const vuetifyLoaderOrderPlugin = {
  apply(compiler) {
    compiler.hooks.afterPlugins.tap('VuetifyLoaderOrderPlugin', () => {
      const { rules } = compiler.options.module;

      const vuetifyAt = rules.findIndex((rule) => mentionsLoader(rule, 'vuetify-loader'));
      const babelAt = rules.findIndex((rule) => mentionsLoader(rule, 'babel-loader'));

      // Nothing to reorder without both of them, and nothing to fix either:
      // the two together are what produces the mixed module.
      if (vuetifyAt === -1 || babelAt === -1 || vuetifyAt > babelAt) {
        return;
      }

      const [vuetifyRule] = rules.splice(vuetifyAt, 1);

      // Loaders run right to left, so a later position means running earlier.
      rules.splice(babelAt, 0, vuetifyRule);
    });
  },
};

module.exports = {
  configureWebpack: {
    performance: {
      hints: false,
    },
    plugins: [
      new webpack.DefinePlugin({
        'process.env.VUE_APP_BUILD_TYPE': JSON.stringify(process.env.VUE_APP_BUILD_TYPE),
      }),
      // Only the test build transpiles modules to CommonJS, and only there do
      // the two loaders conflict. The production build is left as it is.
      ...(process.env.NODE_ENV === 'test' ? [vuetifyLoaderOrderPlugin] : []),
    ],
    devServer: {
      historyApiFallback: true,
      proxy: {
        '^/api': {
          target: 'http://localhost:3000',
        },
      },
    },
  },
  chainWebpack: (config) => {
    config.plugin('html')
      .tap((args) => {
        // eslint-disable-next-line no-param-reassign
        args[0].minify = false;
        return args;
      });
  },
  transpileDependencies: [
    'vuetify',
  ],
  publicPath: './',
  outputDir: '../api/public',
};
