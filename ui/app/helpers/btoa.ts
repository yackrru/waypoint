import { helper } from '@ember/component/helper';

export default helper(([str]) => str && btoa(str));
