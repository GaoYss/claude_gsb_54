<template>
  <el-dialog
    :model-value="modelValue"
    title="退回待处理"
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

    <el-alert type="warning" :closable="false" class="fault-summary">
      退回后故障回到待处理状态, 在办维修记录将被中止, 路灯运行状态随之回落;
      操作人与退回原因会写入处置轨迹。
    </el-alert>

    <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
      <el-form-item label="操作人" prop="operator">
        <el-input v-model="form.operator" maxlength="64" placeholder="填写退回操作人" />
      </el-form-item>
      <el-form-item label="退回原因" prop="reason">
        <el-input
          v-model="form.reason"
          type="textarea"
          :rows="3"
          maxlength="255"
          show-word-limit
          placeholder="例如: 现场核查为线路故障, 原判断灯具破损有误"
        />
      </el-form-item>
    </el-form>

    <template #footer>
      <el-button @click="$emit('update:modelValue', false)">取消</el-button>
      <el-button type="warning" :loading="submitting" @click="handleSubmit">确认退回</el-button>
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

const formRef = ref(null)
const submitting = ref(false)

const createForm = () => ({ operator: '', reason: '' })
const form = reactive(createForm())

const rules = {
  operator: [{ required: true, message: '请填写操作人', trigger: 'blur' }],
  reason: [{ required: true, message: '请填写退回原因', trigger: 'blur' }],
}

function syncForm() {
  Object.assign(form, createForm())
}

async function handleSubmit() {
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  try {
    await faultApi.returnToPending(props.fault.id, { ...form })
    ElMessage.success('故障已退回待处理, 在办维修已中止')
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
