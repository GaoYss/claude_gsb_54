<template>
  <el-dialog
    :model-value="modelValue"
    title="关闭故障"
    width="520px"
    @update:model-value="$emit('update:modelValue', $event)"
    @open="syncForm"
  >
    <el-descriptions v-if="fault" :column="2" border size="small" class="fault-summary">
      <el-descriptions-item label="故障单号">{{ fault.fault_no }}</el-descriptions-item>
      <el-descriptions-item label="路灯编号">{{ fault.lamp_code }}</el-descriptions-item>
      <el-descriptions-item label="故障类型">{{ fault.fault_type || '-' }}</el-descriptions-item>
      <el-descriptions-item label="当前状态">
        <StatusTag :dict="FAULT_STATUS" :value="fault.status" />
      </el-descriptions-item>
    </el-descriptions>

    <el-alert type="info" :closable="false" class="fault-summary">
      关闭后故障不再参与退回等任何流转; 存在在办维修时需先完工或退回。操作人与关闭说明会写入处置轨迹。
    </el-alert>

    <el-form ref="formRef" :model="form" label-width="90px">
      <el-form-item label="操作人" prop="operator">
        <el-input v-model="form.operator" maxlength="64" placeholder="选填, 记录到处置轨迹" />
      </el-form-item>
      <el-form-item label="关闭说明" prop="remark">
        <el-input
          v-model="form.remark"
          type="textarea"
          :rows="3"
          maxlength="255"
          show-word-limit
          placeholder="例如: 现场复核通过 / 误报作废"
        />
      </el-form-item>
    </el-form>

    <template #footer>
      <el-button @click="$emit('update:modelValue', false)">取消</el-button>
      <el-button type="primary" :loading="submitting" @click="handleSubmit">确认关闭</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import StatusTag from '@/components/common/StatusTag.vue'
import { faultApi } from '@/api/fault'
import { FAULT_STATUS } from '@/constants/dict'

const props = defineProps({
  modelValue: { type: Boolean, default: false },
  fault: { type: Object, default: null },
})

const emit = defineEmits(['update:modelValue', 'saved'])

const submitting = ref(false)

const createForm = () => ({ operator: '', remark: '' })
const form = reactive(createForm())

function syncForm() {
  Object.assign(form, createForm())
}

async function handleSubmit() {
  submitting.value = true
  try {
    await faultApi.close(props.fault.id, { ...form })
    ElMessage.success('故障已关闭')
    emit('update:modelValue', false)
    emit('saved')
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.fault-summary {
  margin-bottom: 16px;
}
</style>
